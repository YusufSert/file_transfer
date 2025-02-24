package services

import (
	"context"
	"errors"
	"fmt"
	"github.com/jlaffaye/ftp"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// connReuseStrategy determines how (*DB).conn returns database connections.
type connReuseStrategy uint8

const (
	// alwaysNewConn forces a new connection to the database.
	alwaysNewConn connReuseStrategy = iota
	// cachedOrNewConn returns a cached connection, if available, else waits
	// for one to become available (if MaxOpenConns has been reached) or
	// creates a new database connection.
	cachedOrNewConn
)

// todo : must do create new connection when error in ftp, look sql.Exec method
type FTP struct {

	// Total time waited for new connections
	waitDuration atomic.Int64

	connector connectorFunc

	// numClosed is an atomic counter which represents a total number of
	// closed connections. Stmt.openStmt checks it before cleaning closed
	// connections in Stmt.css.
	numClosed atomic.Uint64

	mu           sync.Mutex // protects following fields
	freeConn     []*ftpConn // free connections ordered by returnedAt oldest to newest
	connRequests map[uint64]chan connRequest
	nextRequest  uint64 // Next key to use in connRequests.
	numOpen      int    // number of opened and pending open connections
	// Used to signal the need for new connections
	// a goroutine running connectionOpener() reads on this chan and
	// maybeOpenNewConnections sends on the can (one send per needed connection)
	openerCh          chan struct{}
	closed            bool
	lastPut           map[*ftpConn]string // stacktrace of last conn's put; debug only
	maxIdleCount      int                 // zero means defaultMaxIdleConns; negative means 0
	maxOpen           int                 // <= 0 means unlimited
	maxLifetime       time.Duration       // maximum amount of time a connection may be reused
	maxIdleTime       time.Duration       // maximum amount of time a connection may be idle before being closed
	cleanerCh         chan struct{}
	waitCount         int64 // Total number of connections waited for.
	maxIdleClosed     int64 // Total number of connections closed due to idle count.
	maxIdleTimeClosed int64 // Total number of connections closed due to idle time.
	maxLifetimeClosed int64 // Total number of connections closed due to max connection lifetime limit.

	stop func() // stop cancels the connection opener.
}

// This is the size of the connectionOpener request chan (FTP.openerCh).
// This value should be larger than the maximum typical value
// used for FTP.maxOpen, If maxOpen is significantly larger than
// connectionRequestQueueSize then it is possible for ALL calls into the *FTP
// to block until the connectionOpener can satisfy the backlog of requests.
var connectionRequestQueueSize = 1000000

var level *slog.LevelVar = &slog.LevelVar{}
var logger *slog.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

func Open(addr, name, password string) (*FTP, error) {
	level.Set(slog.LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	connector, err := openConnector(addr, name, password)
	if err != nil {
		cancel()
		return nil, err
	}

	f := &FTP{
		connector:    connector,
		openerCh:     make(chan struct{}, connectionRequestQueueSize),
		connRequests: make(map[uint64]chan connRequest),
		stop:         cancel,
		maxOpen:      0,
		maxLifetime:  time.Nanosecond,
	}

	go f.connectionOpener(ctx)
	return f, nil
}

type connectorFunc func(context.Context) (*ftp.ServerConn, error)

func openConnector(addr, name, password string) (connectorFunc, error) {
	if len(addr) <= 0 || len(name) <= 0 || len(password) <= 0 {
		return nil, errors.New("ftp: error empty arguments")
	}
	return func(ctx context.Context) (*ftp.ServerConn, error) {
		option := ftp.DialWithContext(ctx)
		conn, err := ftp.Dial(addr, option)
		if err != nil {
			return nil, errBadConn
		}

		err = conn.Login(name, password)
		if err != nil {
			return nil, err
		}

		return conn, nil
	}, nil
}

// maxBadConnRetries is the number of maximum retries if the driver returns
// driver.ErrBadConn to signal a broken connection before forcing a new
// connection to be opened.
const maxBadConnRetries = 2

// todo use circuit breaker with retry
func (s *FTP) retry(fn func(strategy connReuseStrategy) error) error {
	logger.Debug("retry", getGoID())
	for i := int64(0); i < maxBadConnRetries; i++ {
		err := fn(cachedOrNewConn)
		// retry if err is errBadConn
		if err == nil || !errors.Is(err, errBadConn) {
			return err
		}
	}
	logger.Debug("retry alwaysNewConn", getGoID())
	return fn(alwaysNewConn)
}

var errBadConn error = errors.New("ftp-service: bad connection")
var errFTPClosed = errors.New("ftp-service: ftp is closed")

// nextRequestKeyLocked returns the next connection request key.
// It is assumed that nextRequest will not overflow.
func (s *FTP) nextRequestKeyLocked() uint64 {
	logger.Debug("nextRequestKeyLocked")
	next := s.nextRequest
	s.nextRequest++
	return next
}

func (s *FTP) conn(ctx context.Context, strategy connReuseStrategy) (*ftpConn, error) {
	logger.Debug("conn", getGoID())
	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()
		return nil, errFTPClosed
	}
	// Check if the context is expired
	select {
	default:
	case <-ctx.Done():
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	lifetime := s.maxLifetime

	// Prefer a free connection, if possible.
	last := len(s.freeConn) - 1 // pick the freshest one
	if last < 0 {
		logger.Debug("conn no cached conn", getGoID())
	}
	if strategy == cachedOrNewConn && last >= 0 {
		logger.Debug("trying to get cached", getGoID())
		// Reuse the lowes idle time connection so we can close
		// connections which remain idle as son as possible
		conn := s.freeConn[last]
		s.freeConn = s.freeConn[:last]
		conn.inUse = true
		if conn.expired(lifetime) {
			logger.Debug("trying to get cached", getGoID())
			s.maxLifetimeClosed++
			s.numOpen--
			s.mu.Unlock()
			conn.Close()
			return nil, errBadConn
		}
		s.mu.Unlock()

		// todo: maybe implement ftpConn.resetSession

		return conn, nil
	}

	// Out of free connections, or we were asked not to use one. If we're not
	// allowed to open any more connections, make a request and wait. FTP.maxOpen <= 0 means unlimited conn
	fmt.Println("conn ", "maxOpen:", s.maxOpen, "numOpen", s.numOpen)
	if s.maxOpen > 0 && s.numOpen >= s.maxOpen {
		logger.Debug("conn out of cached conn, asking for new one", getGoID())
		// Make the connRequest channel. It' buffered so that the
		// connectionOpener doesn't block while waiting for the req to be read
		req := make(chan connRequest, 1)
		reqKey := s.nextRequestKeyLocked()
		s.connRequests[reqKey] = req
		logger.Debug("conn request free conn key: ", reqKey, getGoID())
		s.waitCount++
		s.mu.Unlock()

		waitStart := time.Now()

		//Timeout the connection request with the context.
		select {
		case <-ctx.Done():
			// Remove the connection request and ensure no value been sent
			// on it after removing.
			s.mu.Lock()
			delete(s.connRequests, reqKey)
			s.mu.Unlock()

			s.waitDuration.Add(int64(time.Since(waitStart)))

			select {
			default:
			case ret, ok := <-req:
				if ok && ret.conn != nil {
					// dont decrement FTP.numOpen here, conn add to freeConn or satisfy the other connRequest
					s.putConn(ret.conn, ret.err, false)
				}
			}
			return nil, ctx.Err()
		case ret, ok := <-req:
			logger.Debug("conn", "req received")
			s.waitDuration.Add(int64(time.Since(waitStart)))

			if !ok {
				return nil, errFTPClosed
			}
			// Only check if the connection is expired if the strategy is cachedOrNewConns.
			// If we require a new connection, just re-use the connection without looking
			// at the expiry time. If it is expired, it will be checked when it is placed
			// back into the connection pool.
			// This prioritizes giving a valid connection to a client over the exact connection
			// lifetime, which could expire exactly after this point anyway.
			if strategy == cachedOrNewConn && ret.err == nil && ret.conn.expired(lifetime) { //ftpConn.expired() returns false if FTP.maxLifetime <= 0, means never expires
				fmt.Println("conn", "new conn expired")
				s.mu.Lock()
				s.maxLifetimeClosed++
				s.numOpen--
				s.mu.Unlock()
				ret.conn.Close()
				return nil, errBadConn
			}
			// if conn == nil, then error is not nil, it will retry or fail
			if ret.conn == nil {
				return nil, ret.err
			}

			return ret.conn, ret.err
		}
	}

	fmt.Println("conn open new connection")
	s.numOpen++ // optimistically
	s.mu.Unlock()
	ci, err := s.connector(ctx)
	if err != nil {
		s.mu.Lock()
		s.numOpen-- // correct for earlier optimism
		s.maybeOpenNewConnections()
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	fc := &ftpConn{
		f:          s,
		createdAt:  time.Now(),
		returnedAt: time.Now(),
		ci:         ci,
		inUse:      true,
	}
	s.mu.Unlock()
	return fc, nil

}

// Assumes ftp.mu is locked.
// If there are connRequests and the connection limit hasn't been reached,
// then tell the connectionOpener to open new connections.
func (s *FTP) maybeOpenNewConnections() {
	logger.Debug("maybeOpenNewConnections")
	numRequests := len(s.connRequests)
	if s.maxOpen > 0 {
		numCanOpen := s.maxOpen - s.numOpen
		if numRequests > numCanOpen {
			numRequests = numCanOpen
		}
	}
	for numRequests > 0 {
		s.numOpen++
		numRequests--
		if s.closed {
			return
		}
		fmt.Println("maybeOpenNewConnections write to openerCh")
		s.openerCh <- struct{}{}
	}
}

// Runs in a separate goroutine, opens new connections when requested.
func (s *FTP) connectionOpener(ctx context.Context) {
	logger.Debug("connectionOpener")
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.openerCh:
			fmt.Println("connectionOpener receive from openerCh")
			s.openNewConnection(ctx)
		}
	}
}

// Open onw new connection
func (s *FTP) openNewConnection(ctx context.Context) {
	logger.Debug("openNewConnection")
	// maybeOpenNewConnections has already executed ftp.numOpen++ before its sent
	// on ftp.openerCh. This function must execute db.numOpen-- if the
	// connection fails or is closed before returning.
	ci, err := s.connector(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		if err == nil {
			ci.Quit()
		}
		s.numOpen--
		return
	}
	if err != nil {
		s.numOpen--
		s.putConnFTPLocked(nil, err)
		s.maybeOpenNewConnections()
		return
	}
	fc := &ftpConn{
		ci:         ci,
		createdAt:  time.Now(),
		returnedAt: time.Now(),
		f:          s,
	}
	if s.putConnFTPLocked(fc, err) {
		fmt.Println("openNewConnection")
		// todo check sql lib if need to implement db.addDepLocked
	} else {
		s.numOpen--
		ci.Quit()
	}
}

// debugGetPut determines whether getConn & putConn calls' stack traces
// are returned for more verbose crashes.
const debugGetPut = false

// putConn adds a connection to the ftp's free pool.
// err is optionally the last error that occurred on this connection.
func (s *FTP) putConn(fc *ftpConn, err error, resetSession bool) {
	logger.Debug("putConn", getGoID())
	if !errors.Is(err, errBadConn) {
		if !fc.validateConnection(resetSession) {
			err = errBadConn
		}
	}

	s.mu.Lock()
	if !fc.inUse {
		s.mu.Unlock()
		if debugGetPut {
			fmt.Printf("putConn(%v) DUBLICATE was: %s\n\nPREVIOS was %s", fc, stack(), s.lastPut[fc])
		}
		panic("ftp-service: connection returned that was never out") // out means: out of FTP.freeConn
	}

	if !errors.Is(err, errBadConn) && fc.expired(s.maxLifetime) {
		fmt.Println("put conn expired")
		s.maxLifetimeClosed++
		err = errBadConn
	}

	if debugGetPut {
		s.lastPut[fc] = stack()
	}
	fmt.Println("putConn setting inUse to false")
	fc.inUse = false
	fc.returnedAt = time.Now()

	if errors.Is(err, errBadConn) {
		// Don't reuse bad connections.
		// Since the conn is considered bad and is being discarded, treat it
		// as closed. Don't decrement open count here, finalClose will
		// take care of that
		s.numOpen--                 // numOpen decrement, finalClose will not implemented
		s.maybeOpenNewConnections() // I should call this bc maybe there is connRequest waiting
		s.mu.Unlock()
		fc.Close()
		return
	}

	added := s.putConnFTPLocked(fc, nil)
	s.mu.Unlock()
	if !added {
		fc.Close()
		return
	}
}

// Satisfy a connRequest or put the driverConn in the idle pool and return true
// or return false.
// putConnFTPLocked will satisfy a connRequest if there is one, or it will
// return the *ftp.ServerConn to the freeConn list if err == nil and the idle
// connection limit will not be exceeded.
// If err != nil, the value of dc is ignored.
// If err == nil, then dc must not equal nil.
// If a connRequest was fulfilled or the *ftp.ServerConn was placed in the
// freeConn list, the true is returned, otherwise false is returned.
func (s *FTP) putConnFTPLocked(fc *ftpConn, err error) bool {
	logger.Debug("putConnFTPLocked", getGoID())
	if s.closed {
		return false
	}
	if s.maxOpen > 0 && s.numOpen > s.maxOpen { // s.numOpen > s.maxOpen aralarında büyüktür olmasının nedeni, maxOpen Set edilebilir. ve NumOpen dan kucuk bir sayı verilebilir.
		return false
	}
	if c := len(s.connRequests); c > 0 {
		fmt.Println("putConnFTPLocked there is conn requests")
		var req chan connRequest
		var reqKey uint64
		for reqKey, req = range s.connRequests {
			break
		}
		delete(s.connRequests, reqKey)
		if err == nil {
			fc.inUse = true
		}

		req <- connRequest{
			conn: fc,
			err:  err,
		}
		slog.Debug("putConnFTPLocked send to conRequestCh key: ", reqKey, getGoID())
		return true
	} else if err == nil && !s.closed {
		logger.Debug("putConnFTPLocked there is no conn request returning conn to freePool", getGoID())
		if s.maxIdleConnsLocked() > len(s.freeConn) {
			s.freeConn = append(s.freeConn, fc)
			s.startCleanerLocked()
			return true
		}
		s.maxIdleClosed++
	}
	return false
}

// startCleanerLocked starts connectionCleaner if needed.
func (s *FTP) startCleanerLocked() {
	logger.Debug("startCleanerLocked")
	if s.numOpen > 0 && s.cleanerCh == nil {
		s.cleanerCh = make(chan struct{}, 1)
		go s.connectionCleaner(s.shortestIdleTimeLocked())
	}
}

func (s *FTP) connectionCleaner(d time.Duration) {
	logger.Debug("connectionCleaner")
	const minInterval = time.Second

	if d < minInterval {
		d = minInterval
	}
	t := time.NewTimer(d)

	for {
		select {
		case <-t.C:
		case <-s.cleanerCh: // maxLifetime was changed or db was closed.
		}

		s.mu.Lock()

		d = s.shortestIdleTimeLocked()
		if s.closed || s.numOpen == 0 || d <= 0 {
			fmt.Println(s.closed, s.numOpen, d)
			s.cleanerCh = nil
			s.mu.Unlock()
			return
		}

		d, closing := s.connectionCleanerRunLocked(d)
		for _, c := range closing {
			fmt.Println("cleaning")
			c.ci.Quit()
			s.numOpen--
		}
		s.mu.Unlock()

		if d < minInterval {
			d = minInterval
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(d)
	}
}

// connectionCleanerRunLocked removes connections that should be closed from
// freeConn and returns them alongside an updated duration to the next check
// if a quicker check is required to ensure connections are checked appropriately.
func (s *FTP) connectionCleanerRunLocked(d time.Duration) (time.Duration, []*ftpConn) {
	logger.Debug("connectionCleanerRunLocked")
	var idleClosing int64
	var closing []*ftpConn
	if s.maxIdleTime > 0 {
		// As freeConn is ordered by returnedAt process
		// in reverse order to minimise the work needed.
		idleSince := time.Now().Add(-s.maxIdleTime)
		last := len(s.freeConn) - 1
		for i := last; i >= 0; i-- {
			c := s.freeConn[i]
			if c.returnedAt.Before(idleSince) {
				i++
				closing = s.freeConn[:i:i]
				s.freeConn = s.freeConn[i:]
				idleClosing = int64(len(closing))
				s.maxIdleClosed += idleClosing
				break
			}
		}

		if len(s.freeConn) > 0 {
			c := s.freeConn[0]
			if d2 := c.returnedAt.Sub(idleSince); d2 < d {
				// Ensure idle connections cleaned up as soon as
				// possible.
				d = d2
			}
		}
	}

	if s.maxLifetime > 0 {
		expiredSince := time.Now().Add(-s.maxLifetime)
		for i := 0; i < len(s.freeConn); i++ {
			c := s.freeConn[i]
			if c.createdAt.Before(expiredSince) {
				closing = append(closing, c)
				last := len(s.freeConn) - 1
				// Use slow delete as order in required to ensure
				// connections are reused least idle time first.
				copy(s.freeConn[i:], s.freeConn[i+1:])
				s.freeConn[last] = nil
				s.freeConn = s.freeConn[:last]
				i--
			} else if d2 := c.createdAt.Sub(expiredSince); d2 < d {
				// Prevent connections sitting the freeConn when they
				// have expired by updating our next deadline d.
				d = d2
			}
		}
		s.maxLifetimeClosed += int64(len(closing)) - idleClosing
	}

	return d, closing
}

// PingContext verifies a connection to the ftp server is still alive,
// establishing a connection if necessary.
func (s *FTP) PingContext(ctx context.Context) error {
	fmt.Println("ping")
	var fc *ftpConn
	var err error

	err = s.retry(func(strategy connReuseStrategy) error {
		fc, err = s.conn(ctx, strategy)
		return err
	})

	if err != nil {
		return err
	}
	withLock(fc, func() {
		err = fc.ci.NoOp()
	})
	fc.releaseConn(err)
	return err
}

// Ping verifies a connection to the database is still alive,
// establishing a connection if necessary.
//
// Ping uses [context.Context] internally; to specify the context, use
// [FTP.PingContext]
func (s *FTP) Ping() error {
	return s.PingContext(context.Background())
}

// FTPStats contains database statistics.
type FTPStats struct {
	MaxOpenConnections int // Maximum number of open connections to the database.

	// Pool Status
	OpenConnections int // The number of established connections both in use and idle.
	InUse           int // The number of connections currently in use.
	Idle            int // The number of idle connections.

	// Counters
	WaitCount         int64         // The total number of connections waited for.
	WaitDuration      time.Duration // The total time blocked waiting for a new connection.
	MaxIdleClosed     int64         // The total number of connections closed due to SetMaxIdleConns.
	MaxIdleTimeClosed int64         // The total number of connections closed due to SetConnMaxIdleTime.
	MaxLifetimeClosed int64         // The total number of connections closed due to SetConnMaxLifeTime.
}

// Stats returns ftp statistics.
func (s *FTP) Stats() FTPStats {
	wait := s.waitDuration.Load()

	s.mu.Lock()
	defer s.mu.Unlock()

	stats := FTPStats{
		MaxOpenConnections: s.maxOpen,
		Idle:               len(s.freeConn),
		OpenConnections:    s.numOpen,
		InUse:              s.numOpen - len(s.freeConn),

		WaitCount:         s.waitCount,
		WaitDuration:      time.Duration(wait),
		MaxIdleClosed:     s.maxIdleClosed,
		MaxIdleTimeClosed: s.maxIdleTimeClosed,
		MaxLifetimeClosed: s.maxLifetimeClosed,
	}
	return stats
}

// ftpConn wraps a *ftp.ServerConn with a mutex, to
// be held during all calls into the Conn.
type ftpConn struct {
	f         *FTP
	createdAt time.Time

	sync.Mutex  // guards following
	ci          *ftp.ServerConn
	needReset   bool // The connection session should be reset before use if true.
	closed      bool
	finalClosed bool //ci.Quit has been called

	// guarded by FTP.mu
	inUse      bool
	returnedAt time.Time // Time the connection was created or returned.
}

func (fc *ftpConn) Close() error {
	fc.Lock()
	if fc.closed {
		fc.Unlock()
		return errors.New("ftp: duplicate ftpConn close")
	}
	fc.closed = true
	fc.ci.Quit()
	return nil
}

// resetSession checks if the underlying ftp connection needs the
// to be reset and if required, resets it.
func (fc *ftpConn) resetSession(ctx context.Context) error {
	fc.Lock()
	defer fc.Unlock()

	if !fc.needReset {
		return nil
	}
	return nil
}

// validateConnection checks if the connection is valid and can
// still be used.
func (fc *ftpConn) validateConnection(needsReset bool) bool {
	fc.Lock()
	defer fc.Unlock()

	err := fc.ci.NoOp()
	if err != nil {
		return false
	}

	return true
}

// if timeout <= 0, never expires
func (fc *ftpConn) expired(timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	return fc.createdAt.Add(timeout).Before(time.Now())
}

func (fc *ftpConn) releaseConn(err error) {
	logger.Debug("releaseConn")
	fmt.Println("releaseConn")
	fc.f.putConn(fc, err, true)

}

func (s *FTP) shortestIdleTimeLocked() time.Duration {
	logger.Debug("shortestIdleTimeLocked")
	if s.maxIdleTime <= 0 {
		fmt.Println(s.maxIdleTime)
		return s.maxLifetime
	}
	if s.maxLifetime <= 0 {
		return s.maxIdleTime
	}
	return min(s.maxIdleTime, s.maxLifetime)
}

// connRequest represents one request for a new connection
// When there are no idle connections available, FTP.conn will create
// a new connRequest and put it on the ftp.connRequests list.
type connRequest struct {
	conn *ftpConn
	err  error
}

const defaultMaxIdleConns = 2

func (s *FTP) maxIdleConnsLocked() int {
	logger.Debug("maxIdleConnsLocked")
	n := s.maxIdleCount
	switch {
	case n == 0:
		return defaultMaxIdleConns
	case n < 0:
		return 0
	default:
		return n
	}
}

func (s *FTP) ListFiles(ctx context.Context, rootPath string) ([]os.FileInfo, error) {
	var fc *ftpConn
	var err error

	err = s.retry(func(strategy connReuseStrategy) error {
		fc, err = s.conn(ctx, strategy)
		return err
	})
	if err != nil {
		return nil, err
	}

	err = fc.ci.ChangeDir(rootPath)
	if err != nil {
		return nil, fmt.Errorf("ftp: dir not exist: %w", err)
	}

	walker := fc.ci.Walk("./")
	var files []os.FileInfo

	for walker.Next() {
		err := walker.Err()
		if err != nil {
			return nil, err
		}

		// skip dir
		stat := walker.Stat()
		if stat.Type == ftp.EntryTypeFolder {
			continue
		}

		fInfo := FileInfo{
			name: walker.Path(),
			size: int64(stat.Size),
			typ:  stat.Type,
			time: stat.Time,
		}
		files = append(files, fInfo)
	}
	fc.releaseConn(err)
	return files, nil
}

func stack() string {
	logger.Debug("stack")
	var buf [2 << 10]byte
	return string(buf[:runtime.Stack(buf[:], false)])

}

// withLock runs while holding lk.
func withLock(lk sync.Locker, fn func()) {
	logger.Debug("withLock")
	lk.Lock()
	defer lk.Unlock()
	fn()
}

/*

func (s *FTP) Login(user, password string) error {
	s.mu.Lock()
	defer s.m.Unlock()
	err := s.conn.Login(user, password)
	if err != nil {
		return fmt.Errorf("filetransfer-service login failure %w", err)
	}
	return nil
}




func isNotFoundErr(err error) bool {
	return strings.Contains(err.Error(), "550")
}

func (s *FTP) Fetch(i os.FileInfo) (io.ReadCloser, error) {
	s.m.Lock()
	defer s.m.Unlock()
	return s.fetch(i)
}

func (s *FTP) Delete(info os.FileInfo) error {
	s.m.Lock()
	defer s.m.Unlock()
	err := s.conn.Delete(info.Name())
	if err != nil {
		if isNotFoundErr(err) {
			return ErrNotFound
		}
		return err
	}
	fmt.Println(err)

	return nil
}

func (s *FTP) fetch1(i FileInfo) (*ftp.Response, error) {
	s.m.Lock()
	defer s.m.Unlock()
	var res *ftp.Response
	var err error
	baseBOff, maxBOff := time.Second, time.Minute

	for r := 0; ; r++ {
		if baseBOff > maxBOff {
			baseBOff = maxBOff
		}
		res, err = s.conn.Retr(i.name)

		if err == nil {
			return res, nil
		} else if isNotFoundErr(err) {
			return nil, fmt.Errorf("filetransfer-service file not found %w", err)
		} else if r >= 5 {
			return nil, fmt.Errorf("filetransfer-service max retry error file:%s %w", i.name, err)
		}
		time.Sleep(baseBOff)
		slog.Error("retrying", "file", i.Name, "error", err)
		baseBOff <<= 1
	}
}
func (s *FTP) fetch(i os.FileInfo) (*ftp.Response, error) {
	s.m.Lock()
	defer s.m.Unlock()
	var res *ftp.Response
	var err error
	res, err = s.conn.Retr(i.Name())

	return res, err
}

func (s *FTP) Copy(dst io.Writer, i os.FileInfo) (int64, error) {
	s.m.Lock()
	defer s.m.Unlock()
	var res *ftp.Response
	var err error
	res, err = s.conn.Retr(i.Name())
	if err != nil {
		return 0, err
	}
	defer res.Close()

	n, err := io.Copy(dst, res)
	if err != nil {
		return n, err
	}

	return n, nil
}

func (s *FTP) Store(i os.FileInfo, r io.Reader) error {
	s.m.Lock()
	defer s.m.Unlock()
	err := s.conn.Stor(i.Name(), r)
	if err != nil {
		return err
	}

	return nil
}

func (s *FTP) SendHeartBeat() error {
	s.m.Lock()
	defer s.m.Unlock()
	return s.conn.NoOp()
}
*/

type FileInfo struct {
	name string
	size int64
	typ  ftp.EntryType
	time time.Time
}

func Info(name string) os.FileInfo {
	return FileInfo{name: name}
}

func (f FileInfo) Name() string {
	return f.name
}
func (f FileInfo) Size() int64 {
	return f.size
}
func (f FileInfo) Mode() os.FileMode {
	panic("not implemented")
}

func (f FileInfo) ModTime() time.Time {
	return f.time
}

func (f FileInfo) IsDir() bool {
	return f.typ == ftp.EntryTypeFolder
}

func (f FileInfo) Sys() any {
	panic("not implemented")
}

var (
	ErrNotFound = errors.New("no such file or directory")
)

func getGoID() string {
	buf := make([]byte, 11)
	runtime.Stack(buf, false)
	return string(buf)
}
