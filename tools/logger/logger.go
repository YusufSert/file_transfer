package logger

import (
    "log"
    "log/slog"
    "os"
)

type Logger struct {
    Logger *slog.Logger
    Level  *slog.LevelVar

    w *os.File
}

func NewLogger(filePath string) (*Logger, error) {
    w, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
    if err != nil {
        log.Fatal(err)
    }

    level := &slog.LevelVar{}
    level.Set(slog.LevelDebug)
    logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))

    a := &Logger{
        Logger: logger,
        Level:  level,
        w:      w,
    }

    return a, nil
}

func (l *Logger) SetLevel(level slog.Level) {
    l.Level.Set(level)
}
func (l *Logger) Close() {
    l.w.Close()
}

/*
func (a *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lvl := a.Level.Level() // -4, 0, 4, 8
	switch lvl {           //clean
	case slog.LevelDebug:
		lvl = slog.LevelInfo
	case slog.LevelInfo:
		lvl = slog.LevelWarn
	case slog.LevelWarn:
		lvl = slog.LevelError
	case slog.LevelError:
		lvl = slog.LevelDebug
	}
	a.Level.Set(lvl)
	stat, _ := os.Stat(a.filePath)

	info := struct {
		Level           string    `json:"log_level"`
		CurReadPosition uint64    `json:"cur_read_position"`
		FileName        string    `json:"file_name"`
		FileSize        int64     `json:"file_size"`
		FileModTime     time.Time `json:"file_mod_time"`
	}{
		Level:           a.Level.String(),
		FileName:        stat.Name(),
		FileSize:        stat.Size(),
		FileModTime:     stat.ModTime(),
		CurReadPosition: a.position,
	}

	json.NewEncoder(w).Encode(&info)
}

func (a *Logger) runServer() {
	err := http.ListenAndServe("localhost:8000", a)
	if err != nil {
		a.Logger.Error("http server not  running", "err", err)
		a.err = fmt.Errorf("log-agent: error creating endpoint for changing logLelel %w", err)
		a.Close()
	}
}

type Record struct {
	Time  time.Time
	Level string
	Msg   string
	Data  map[string]any
}

*/
