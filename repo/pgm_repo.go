package repo

import (
	"database/sql"
	"log/slog"
)

type PGMRepo struct {
	db *sql.DB
	l  *slog.Logger
}
