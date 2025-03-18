package repo

import (
    "context"
    "database/sql"
    "errors"
    "fmt"
)

type UsersRepository struct {
	db *sql.DB
}

func NewUsersRepository(db *sql.DB) *UsersRepository {
	return &UsersRepository{db: db}
}

func (r *UsersRepository) FindById(ctx context.Context, id string) (*User, error) {
	q := `SELECT * FROM users WHERE id = $1`

	row := r.db.QueryRowContext(ctx, q, id)

	var u User
	err := row.Scan(&u.Id, &u.Username, &u.Phone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("user_repository: users not found %w", err)
	}

	if err = row.Err(); err != nil {
		return nil, err
	}

	return &u, nil

}

func (r *UsersRepository) FindWithPasswordById(ctx context.Context, id string) {
	q := `SELECT u.id, u.username, u.phone, s.password FROM user u inner join secret s on(u.id = s.user_id) WHERE u.id = $1`
	row := r.QueryRowContext(ctx, q, id)

	var u UserWithPassWord
	err := row.Scan(&u.Id, &u.Username, &u.Phone, &u.Password)
	if err != nil {

	}
}

func (r *UsersRepository) Add(ctx context.Context, u UserWithPassWord) error {
	q := `CALL add_user($1, $2, $3, $4)`
	_, err := r.ExecContext(ctx, q, u.Id, u.Username, u.Phone, u.Password)
	if err != nil {
		return err
	}

	return nil
}

type User struct {
	Id       string   `json:"id"`
	Username string   `json:"username"`
	Phone    string   `json:"phone"`
	Roles    []string `json:"roles,omitempty"`
}
type UserWithPassWord struct {
	User
	Password string `json:"password"`
}
