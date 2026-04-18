// Package store isolates SQL from the rest of the code.
//
// Why a separate store layer?
//   - Resolvers stay focused on input validation and wiring, not SQL strings.
//   - When you eventually want to swap Postgres for something else, or add
//     caching, you change one package.
//   - It's trivially testable: a `StoreIface` interface would let you mock it.
//     For brevity I haven't added the interface yet — the README notes this.
package store

import (
	"context"
	"errors"
	"strings"

	"taskflow/internal/db"
	"taskflow/internal/model"
)

type Store struct {
	DB *db.Pool
}

func New(p *db.Pool) *Store { return &Store{DB: p} }

// ErrNotFound is returned when a lookup finds zero rows. Resolvers translate
// this to a user-facing "not found" error. Having a sentinel error lets
// callers use `errors.Is(err, store.ErrNotFound)`.
var ErrNotFound = errors.New("not found")

// ---------- users ----------

func (s *Store) CreateUser(ctx context.Context, email, name, hash string) (*model.User, error) {
	u := &model.User{Email: email, Name: name, PasswordHash: hash}
	const q = `INSERT INTO users (email, name, password_hash)
	           VALUES ($1, $2, $3) RETURNING id, created_at`
	err := s.DB.QueryRow(ctx, q, email, name, hash).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "23505") {
			// Postgres unique_violation. We translate it into a domain-
			// meaningful error so resolvers don't need to know SQLSTATE codes.
			return nil, errors.New("email already registered")
		}
		return nil, err
	}
	return u, nil
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*model.User, error) {
	u := &model.User{}
	const q = `SELECT id, email, name, password_hash, created_at
	           FROM users WHERE email = $1`
	err := s.DB.QueryRow(ctx, q, email).
		Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, ErrNotFound
	}
	return u, nil
}

func (s *Store) UserByID(ctx context.Context, id string) (*model.User, error) {
	u := &model.User{}
	const q = `SELECT id, email, name, created_at FROM users WHERE id = $1`
	if err := s.DB.QueryRow(ctx, q, id).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt); err != nil {
		return nil, ErrNotFound
	}
	return u, nil
}

// ---------- projects ----------

func (s *Store) CreateProject(ctx context.Context, ownerID, name string, desc *string) (*model.Project, error) {
	p := &model.Project{Name: name, Description: desc, OwnerID: ownerID}
	const q = `INSERT INTO projects (name, description, owner_id)
	           VALUES ($1, $2, $3) RETURNING id, created_at`
	if err := s.DB.QueryRow(ctx, q, name, desc, ownerID).Scan(&p.ID, &p.CreatedAt); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) ProjectsByOwner(ctx context.Context, ownerID string) ([]*model.Project, error) {
	const q = `SELECT id, name, description, owner_id, created_at
	           FROM projects WHERE owner_id = $1 ORDER BY created_at DESC`
	rows, err := s.DB.Query(ctx, q, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Project
	for rows.Next() {
		p := &model.Project{}
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.OwnerID, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ProjectByID(ctx context.Context, id string) (*model.Project, error) {
	p := &model.Project{}
	const q = `SELECT id, name, description, owner_id, created_at
	           FROM projects WHERE id = $1`
	if err := s.DB.QueryRow(ctx, q, id).Scan(&p.ID, &p.Name, &p.Description, &p.OwnerID, &p.CreatedAt); err != nil {
		return nil, ErrNotFound
	}
	return p, nil
}

// ---------- tasks ----------

func (s *Store) CreateTask(ctx context.Context, projectID, title string, desc, assigneeID *string) (*model.Task, error) {
	t := &model.Task{
		ProjectID: projectID, Title: title, Description: desc,
		AssigneeID: assigneeID, Status: model.StatusTodo,
	}
	const q = `INSERT INTO tasks (project_id, title, description, assignee_id)
	           VALUES ($1, $2, $3, $4)
	           RETURNING id, status, created_at, updated_at`
	err := s.DB.QueryRow(ctx, q, projectID, title, desc, assigneeID).
		Scan(&t.ID, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) TaskByID(ctx context.Context, id string) (*model.Task, error) {
	t := &model.Task{}
	const q = `SELECT id, project_id, title, description, status,
	                  assignee_id, created_at, updated_at
	           FROM tasks WHERE id = $1`
	err := s.DB.QueryRow(ctx, q, id).Scan(
		&t.ID, &t.ProjectID, &t.Title, &t.Description, &t.Status,
		&t.AssigneeID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, ErrNotFound
	}
	return t, nil
}

func (s *Store) TasksByProject(ctx context.Context, projectID string) ([]*model.Task, error) {
	const q = `SELECT id, project_id, title, description, status,
	                  assignee_id, created_at, updated_at
	           FROM tasks WHERE project_id = $1 ORDER BY created_at`
	rows, err := s.DB.Query(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Task
	for rows.Next() {
		t := &model.Task{}
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Description, &t.Status,
			&t.AssigneeID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTask performs a full-row update. We pass the whole Task in; the
// resolver is responsible for merging partial input onto the existing record.
func (s *Store) UpdateTask(ctx context.Context, t *model.Task) error {
	const q = `
		UPDATE tasks
		   SET title = $2, description = $3, status = $4, assignee_id = $5,
		       updated_at = NOW()
		 WHERE id = $1
		 RETURNING updated_at`
	return s.DB.QueryRow(ctx, q, t.ID, t.Title, t.Description, t.Status, t.AssigneeID).Scan(&t.UpdatedAt)
}

// DeleteTaskByOwner deletes only if the caller owns the parent project.
// Combining the auth check into the SQL means one round trip and an atomic
// check — no TOCTOU race where someone changes ownership between two queries.
func (s *Store) DeleteTaskByOwner(ctx context.Context, taskID, ownerID string) (bool, error) {
	const q = `
		DELETE FROM tasks
		 WHERE id = $1
		   AND project_id IN (SELECT id FROM projects WHERE owner_id = $2)`
	tag, err := s.DB.Exec(ctx, q, taskID, ownerID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
