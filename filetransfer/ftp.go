package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"github.com/jlaffaye/ftp"
	"io"
	"os"
	"path"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type FTP struct {

	// Total time waited for new connections
	waitDuration atomic.Int64

	// numClosed is an atomic counter which represents a total number of
	// closed connections. Stmt.openStmt checks it before cleaning closed
	// connections in Stmt.css.
	numClosed atomic.Uint64

	connectServer connectorFunc

	mu           sync.Mutex // protects the following fields
	freeConn     []*ftpConn // free connections ordered by returnedAt oldest to newest
	connRequests map[uint64]chan connRequest
	nextRequest  uint64 // Next key to use in connRequests.
	numOpen      int    // number of opened and pending open connections
	// Used to signal the need for new connections
	// a goroutine running connectionOpener() reads on this chan and
	// maybeOpenNewConnections sends on the can (one send per needed connection)
	openerCh          chan struct{}
	closed            bool
	lastPut           map[*ftpConn]string // stacktrace of last conn's put; debug only
	maxIdleCount      int                 // zero means defaultMaxIdleConns; negative means 0
	maxOpen           int                 // <= 0 means unlimited
	maxLifetime       time.Duration       // maximum amount of time a connection may be reused
	maxIdleTime       time.Duration       // maximum amount of time a connection may be idle before being closed
	cleanerCh         chan struct{}
	waitCount         int64 // The Total number of connections waited for.
	maxIdleClosed     int64 // Total number of connections closed due to idle count.
	maxIdleTimeClosed int64 // Total number of connections closed due to idle time.
	maxLifetimeClosed int64 // Total number of connections closed due to max connection lifetime limit.

	stop func() // stop cancels the connection opener.
}

// This is the size of the connectionOpener request chan (FTP.openerCh).
// This value should be larger than the maximum typical value
// used for FTP.maxOpen, If maxOpen is significantly larger than
// connectionRequestQueueSize then it is possible for All cals into the *FTP
// to block until the connectionOpener can satisfy the backlog of requests.
var connectionRequestQueueSize = 1000000

func Open(addr, name, password string) (*FTP, error) {
	ctx, cancel := context.WithCancel(context.Background())
	connector, err := openConnector(addr, name, password)
	if err != nil {
		cancel()
		return nil, err
	}

	f := &FTP{
		connectServer: connector,
		openerCh:      make(chan struct{}, connectionRequestQueueSize),
		connRequests:  make(map[uint64]chan connRequest),
		lastPut:       make(map[*ftpConn]string),
		stop:          cancel,
	}

	go f.connectionOpener(ctx)
	return f, nil
}

// connReuseStrategy determines how (FTP).conn returns ftp connections
type connReuseStrategy uint8

const (
	// alwaysNewConn forces a new connection to the database.
	alwaysNewConn connReuseStrategy = iota
	// cachedOrNewConn returns a cached connection, if available, else waits
	// for one to become available (if MaxOpenConns has been reached) or
	// creates a new database connection.
	cachedOrNewConn
)

// nextRequestKeyLocked returns the next connection request key.
// It is assumed that nextRequest will not overflow.
func (s *FTP) nextRequestKeyLocked() uint64 {
	next := s.nextRequest
	s.nextRequest++
	return next
}

// conn returns a newly-opened or cached *ftoConn
func (s *FTP) conn(ctx context.Context, strategy connReuseStrategy) (*ftpConn, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errFTPClosed
	}
	// Check if the context is expired.
	select {
	default:
	case <-ctx.Done():
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	lifetime := s.maxLifetime

	// Prefer a free connection, if possible.
	last := len(s.freeConn) - 1
	if strategy == cachedOrNewConn && last >= 0 {
		// Reuse the lowest idle time connection se we can close
		// connection which remain idle as soon as possible.
		conn := s.freeConn[last]
		s.freeConn = s.freeConn[:last]
		conn.inUse = true
		if conn.expired(lifetime) {
			s.maxLifetimeClosed++
			s.mu.Unlock()
			conn.close()
			return nil, errBadConn
		}
		s.mu.Unlock()
		return conn, nil
	}

	// Out of free connections or we were asked not to use one. If we're not
	// allowed to open any more connections, make a request and wait.
	if s.maxOpen > 0 && s.numOpen >= s.maxOpen {
		// Make the connRequest channel, It's buffered so that the
		// connectionOpener doesn't block while waiting for the req to be read.
		req := make(chan connRequest, 1)
		reqKey := s.nextRequestKeyLocked()
		s.waitCount++
		s.mu.Unlock()

		waitStart := time.Now()

		// Timeout the connection request with the context.
		select {
		case <-ctx.Done():
			// Remove the connection request and ensure no values has been sent
			// on it after removing
			s.mu.Lock()
			delete(s.connRequests, reqKey)
			s.mu.Unlock()

			s.waitDuration.Add(int64(time.Since(waitStart)))

			select {
			default:
			case ret, ok := <-req:
				if ok && ret.conn != nil {
					s.putConn(ret.conn, ret.err)
				}
			}
			return nil, ctx.Err()
		case ret, ok := <-req:
			s.waitDuration.Add(int64(time.Since(waitStart)))

			if !ok {
				return nil, errFTPClosed
			}
			// Only check if the connection is expired if the strategy is cachedOrNewConns.
			// If we require a new connection, just re-use the connection without looking
			// at the expiry time. If it is expired, it will be checked when it is placed
			// back into the connection pool. FTP.putConn().
			// This prioritizes giving a valid connection to a client over the exact connection
			// lifetime, which could expire exactly after this point anyway.
			if strategy == cachedOrNewConn && ret.err == nil && ret.conn.expired(lifetime) {
				s.mu.Lock()
				s.maxLifetimeClosed++
				s.mu.Unlock()
				ret.conn.close() // decrements ftp.numOpen--
				return nil, errBadConn
			}
			if ret.conn == nil {
				return nil, ret.err //openNewConnection already decrement the ftp.numOpen--
			}
			return ret.conn, ret.err
		}
	}

	s.numOpen++ // optimistically
	s.mu.Unlock()
	ci, err := s.connectServer(ctx)
	if err != nil {
		s.mu.Lock()
		s.numOpen-- // correct for earlier optimism
		//s.maybeOpenNewConnections()
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	fc := &ftpConn{
		s:          s,
		createdAt:  time.Now(),
		returnedAt: time.Now(),
		ci:         ci,
		inUse:      true,
	}
	s.mu.Unlock()
	return fc, nil
}

// debugGetPut determines whether getConn & putConn calks' stack traces
// are returned for more verbose crashes.
const debugGetPut = true

// putConn adds a connection to the ftp's free pool.
// err is optionally the last error that occurred on this connection.
func (s *FTP) putConn(fc *ftpConn, err error) {
	if !errors.Is(err, errBadConn) {
		if !fc.validateConnection() {
			err = errBadConn
		}
	}
	s.mu.Lock()
	if !fc.inUse {
		if debugGetPut {
			fmt.Printf("putConn(%v) DUPLICATE was: %s\n\nPREVIOUS was: %s", fc, stack(), s.lastPut[fc])
		}
		panic("sql: connection returned that was never out")
	}

	if !errors.Is(err, errBadConn) && fc.expired(s.maxLifetime) {
		s.maxIdleClosed++
		err = errBadConn
	}
	if debugGetPut {
		s.lastPut[fc] = stack()
	}
	fc.inUse = false
	fc.returnedAt = time.Now()

	if errors.Is(err, errBadConn) {
		// Don't reuse bad connections.
		// Since the conn is considered bad and being discarded, treat it
		// as closed, Don't decrement the open count here, Close will
		// take care of that.
		//s.maybeOpenNewConnections()
		s.mu.Unlock()
		fc.close()
		return
	}

	added := s.putConnFTPLocked(fc, nil)
	s.mu.Unlock()

	if !added {
		fc.close()
	}
}

// Satisfy a connRequest or put the ftpConn id the idle pool and return true
// or return false.
// putConnFTPLocked will satisfy a connRequest if there is one, or it will
// return the *ftpConn to the freeConn list if err == nil and the idle
// connection limit will not be exceeded.
// If err != nil, the value of fc is ignored.
// If err == nil, then fc must not equal nil.
// If a connRequest was fulfilled or the *ftpConn was placed in the
// freeConn list, then true is returned, otherwise false is returned.
func (s *FTP) putConnFTPLocked(fc *ftpConn, err error) bool {
	if s.closed {
		return false
	}
	if s.maxOpen > 0 && s.numOpen > s.maxOpen {
		return false
	}
	if c := len(s.connRequests); c > 0 {
		var req chan connRequest
		var reqKey uint64
		for reqKey, req = range s.connRequests {
			break
		}
		delete(s.connRequests, reqKey) // Remove from pending requests.
		if err == nil {
			fc.inUse = true
		}
		req <- connRequest{
			conn: fc,
			err:  err,
		}
		return true
	} else if err == nil && !s.closed {
		if s.maxIdleConnsLocked() > len(s.freeConn) {
			s.freeConn = append(s.freeConn, fc)
			s.startCleanerLocked()
			return true
		}
		s.maxIdleClosed++
	}
	return false
}

// maxBadConnRetries is the number of maximum retries if the ftp returns
// errBadConn to signal a broken connection before forcing a new
// connection to be opened.
const maxBadConnRetries = 2

func (s *FTP) retry(fn func(strategy connReuseStrategy) error) error {
	for i := 0; i < maxBadConnRetries; i++ {
		err := fn(cachedOrNewConn)
		// retry if err is errBadConn
		if err == nil || !errors.Is(err, errBadConn) {
			return err
		}
	}

	return fn(alwaysNewConn)
}

// FTPStats contains ftp statistics
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
	MaxLifetimeClosed int64         // The total number of connections closed due to SetConnMaxLifetime.
}

// Stats returns ftp statistics.
func (s *FTP) Stats() FTPStats {
	wait := s.waitDuration.Load()

	s.mu.Lock()
	defer s.mu.Unlock()

	stats := FTPStats{
		MaxOpenConnections: s.maxOpen,

		Idle:            len(s.freeConn),
		OpenConnections: s.numOpen,
		InUse:           s.numOpen - len(s.freeConn),

		WaitCount:         s.waitCount,
		WaitDuration:      time.Duration(wait),
		MaxIdleClosed:     s.maxIdleClosed,
		MaxIdleTimeClosed: s.maxIdleTimeClosed,
		MaxLifetimeClosed: s.maxLifetimeClosed,
	}
	return stats
}

// Assumes ftp.mu is locked.
// If there are connRequests and the connection limit hasn't been reached,
// then tell the connectionOpener to open new connections.
func (s *FTP) maybeOpenNewConnections() {
	numRequests := len(s.connRequests)
	if s.maxOpen > 0 {
		numCanOpen := s.maxOpen - s.numOpen
		if numRequests > numCanOpen {
			numRequests = numCanOpen
		}
	}
	for numRequests > 0 {
		s.numOpen++ // optimistically
		numRequests--
		if s.closed {
			return
		}
		s.openerCh <- struct{}{}
	}
}

// Runs is a separate goroutine, opens new connections when requested.
func (s *FTP) connectionOpener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.openerCh:
			s.openNewConnection(ctx)
		}
	}
}

// Open one new connection.
func (s *FTP) openNewConnection(ctx context.Context) {
	// maybeOpenNewConnections has already executed ftp.numOpen++ before it sent
	// on ftp.openerCh. This function must execute ftp.numOpen-- if the
	// connection fails or is closed before returning.
	ci, err := s.connectServer(ctx)
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
		s:          s,
		createdAt:  time.Now(),
		returnedAt: time.Now(),
		ci:         ci,
	}
	if !s.putConnFTPLocked(fc, err) {
		s.numOpen--
		ci.Quit()
	}
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
			return nil, errBadConn
		}

		return conn, nil
	}, nil
}

const defaultMaxIdleConns = 2

// Used with putConnFTPLocked to limit the idleConns
func (s *FTP) maxIdleConnsLocked() int {
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

func (s *FTP) shortestIdleTimeLocked() time.Duration {
	if s.maxIdleTime <= 0 {
		return s.maxLifetime
	}
	if s.maxLifetime <= 0 {
		return s.maxLifetime
	}
	return min(s.maxIdleTime, s.maxLifetime)
}

// SetMaxIdleConns sets the maximum number of connections in the idle
// connection pool.
//
// If MaxOpenConns is greater than 0 but less than new MaxIdleConns,
// then the new MaxIdleConns will be reduced to match the MaxOpenConns limit.
//
// If n <= 0, no idle connections are retained.
func (s *FTP) SetMaxIdleConns(n int) {
	s.mu.Lock()
	if n > 0 {
		s.maxIdleCount = n
	} else {
		// No idle connections
		s.maxIdleCount = -1
	}

	// Make sure maxIdle doesn't exceed maxOpen
	if s.maxOpen > 0 && s.maxIdleConnsLocked() > s.maxOpen {
		s.maxIdleCount = s.maxOpen
	}
	var closing []*ftpConn
	idleCount := len(s.freeConn)
	maxIdle := s.maxIdleConnsLocked()
	if idleCount > maxIdle {
		closing = s.freeConn[:maxIdle]
		s.freeConn = s.freeConn[:maxIdle]
	}
	s.maxIdleClosed += int64(len(closing))
	s.mu.Unlock()
	for _, c := range closing {
		c.close() // close() will decrement ftp.numOpen--
	}
}

// startCleanerLocked starts connectionCleaner if needed.
func (s *FTP) startCleanerLocked() {
	if (s.maxLifetime > 0 || s.maxIdleTime > 0) && s.numOpen > 0 && s.cleanerCh == nil {
		s.cleanerCh = make(chan struct{}, 1)
		go s.connectionCleaner(s.shortestIdleTimeLocked())
	}
}

func (s *FTP) connectionCleaner(d time.Duration) {
	const minInterval = time.Second

	if d < minInterval {
		d = minInterval
	}
	t := time.NewTimer(d)

	for {
		select {
		case <-t.C:
		case <-s.cleanerCh: // maxLifetime was changed or ftp was closed.
		}

		s.mu.Lock()

		d = s.shortestIdleTimeLocked()
		if s.closed || s.numOpen == 0 || d <= 0 {
			s.cleanerCh = nil
			s.mu.Unlock()
			return
		}

		d, closing := s.connectionCleanerRunLocked(d)
		s.mu.Unlock()
		for _, c := range closing {
			c.close()
		}

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
// freeConn and returns them along side an updated duration to next check.
// if a quicker check is required to ensure connections are checked appropriately.
func (s *FTP) connectionCleanerRunLocked(d time.Duration) (time.Duration, []*ftpConn) {
	var idleClosing int64
	var closing []*ftpConn
	if s.maxIdleTime > 0 {
		// As freeConn is ordered by returnedAt process
		// in revers order to minimize the work needed.
		idleSince := time.Now().Add(-s.maxIdleTime)
		last := len(s.freeConn) - 1
		for i := last; i >= 0; i-- {
			c := s.freeConn[i]
			if c.returnedAt.Before(idleSince) {
				i++
				closing = s.freeConn[:i:i]
				s.freeConn = s.freeConn[i:]
				idleClosing = int64(len(closing))
				s.maxIdleTimeClosed += idleClosing
				break
			}
		}

		if len(s.freeConn) > 0 {
			c := s.freeConn[0]
			if d2 := c.returnedAt.Sub(idleSince); d2 < d {
				// Ensure idle connections are cleaned up as soon as
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

				last := len(s.freeConn) - 1 // we are using slow delete to dont mix the order of freeConn
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

// connRequest represents one request for a new connection
// When there are no idle connections available, FTP.conn will create
// a new connRequest and put it on the ftp.connRequests list.
type connRequest struct {
	conn *ftpConn
	err  error
}

// ftpConn wraps a *ftp.ServerConn with a mutex, to
// be held during all calls into the ServerConn.
type ftpConn struct {
	s         *FTP
	createdAt time.Time

	sync.Mutex // guards following
	ci         *ftp.ServerConn
	needReset  bool // The connection session should be reset before use if true
	closed     bool

	// guard by FTP.mu
	inUse      bool
	returnedAt time.Time // Time the connection was created or returned
	onPut      []func()  // code (with FTP.mu held) run when conn is next returned
}

// Close will decrement FTP.numOpen
func (fc *ftpConn) close() error {
	err := fc.ci.Quit()

	fc.s.mu.Lock()
	fc.s.numOpen--
	//fc.s.maybeOpenNecConnections()
	fc.s.mu.Unlock()

	fc.s.numClosed.Add(1)
	return err
}

// validateConnection checks if the connection is valid and can
// still be used.
func (fc *ftpConn) validateConnection() bool {
	fc.Lock()
	defer fc.Unlock()

	err := fc.ci.NoOp()
	if err != nil {
		return false
	}
	return true
}

func (fc *ftpConn) expired(timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	return fc.createdAt.Add(timeout).Before(time.Now())
}

func (fc *ftpConn) releaseConn(err error) {
	fc.s.putConn(fc, err)
}

func stack() string {
	var buf [2 << 10]byte
	return string(buf[:runtime.Stack(buf[:], false)])
}

// withLock runs while holding lk.
func withLock(lk sync.Locker, fn func()) {
	lk.Lock()
	defer lk.Unlock()
	fn()
}

var errBadConn = errors.New("fpt-service: bad connection")
var errFTPClosed = errors.New("fpt-service: ftp is closed")

// PingContext verifies a connection to the ftp server is still alive,
func (s *FTP) PingContext(ctx context.Context) error {
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

// ListFilesContext returns file-infos from the given path
func (s *FTP) ListFilesContext(ctx context.Context, rootPath string) ([]os.FileInfo, error) {
	var fc *ftpConn
	var err error

	err = s.retry(func(strategy connReuseStrategy) error {
		fc, err = s.conn(ctx, strategy)
		return err
	})
	if err != nil {
		return nil, err
	}

	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	err = fc.ci.ChangeDir(rootPath) // needs context
	if err != nil {
		return nil, fmt.Errorf("ftp: dir not exist: %w", err)
	}

	walker := fc.ci.Walk("./")
	var files []os.FileInfo

	for walker.Next() {
		select {
		default:
		case <-ctx.Done():
			return files, ctx.Err()
		}

		err := walker.Err()
		if err != nil {
			return nil, err
		}

		// skip dir
		stat := walker.Stat()

		if stat.Type == ftp.EntryTypeFolder {
			walker.SkipDir()
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

// FTP 550 no such file or directory”
//todo: return errNotFound if ftp returns 550 code.

// Copy copies the file to given io.writer
func (s *FTP) Copy(dst io.Writer, path string) (int64, error) {
	var fc *ftpConn
	var err error

	err = s.retry(func(strategy connReuseStrategy) error {
		fc, err = s.conn(context.Background(), strategy)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("ftp: error getting conn from the pool %w", err)
	}

	var src *ftp.Response
	withLock(fc, func() {
		src, err = fc.ci.Retr(path)
	})
	if err != nil {
		return 0, fmt.Errorf("ftp: error retriving file: %s %w", path, err)
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return 0, fmt.Errorf("ftp: error copying file: %s %w", path, err)
	}
	fc.releaseConn(err)
	return n, err
}

// Delete deletes the file from the ftp server
func (s *FTP) Delete(p string) error {
	p = path.Clean(p)
	var fc *ftpConn
	var err error

	err = s.retry(func(strategy connReuseStrategy) error {
		fc, err = s.conn(context.Background(), strategy)
		return err
	})
	if err != nil {
		return err
	}
	withLock(fc, func() {
		err = fc.ci.Delete(p)
	})
	if err != nil {
		return fmt.Errorf("ftp: error deleting file from %s %w", p, err)
	}
	fc.releaseConn(err)
	return nil
}

func (s *FTP) Store(p string, r io.Reader) error {
	p = path.Clean(p)
	var fc *ftpConn
	var err error

	err = s.retry(func(strategy connReuseStrategy) error {
		fc, err = s.conn(context.Background(), strategy)
		return err
	})
	if err != nil {
		return err
	}

	withLock(fc, func() {
		err = fc.ci.Stor(p, r)
	})
	if err != nil {
		return fmt.Errorf("ftp: error storing file to %s %w", p, err)
	}
	fc.releaseConn(err)
	return nil
}

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
