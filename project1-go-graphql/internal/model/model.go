// Package model holds the domain structs. These are the shape of rows coming
// out of the DB and the shape the GraphQL layer serializes to JSON.
//
// Keeping DB-row structs and GraphQL-response structs unified is fine at this
// scale. Once you start needing computed fields, aggregations, or a public
// API that diverges from storage, split into `model.UserRow` and
// `api.User` so the two can evolve independently.
package model

import "time"

type TaskStatus string

const (
	StatusTodo       TaskStatus = "TODO"
	StatusInProgress TaskStatus = "IN_PROGRESS"
	StatusDone       TaskStatus = "DONE"
)

func (s TaskStatus) IsValid() bool {
	switch s {
	case StatusTodo, StatusInProgress, StatusDone:
		return true
	}
	return false
}

type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"createdAt"`
	PasswordHash string    `json:"-"` // never serialized; `-` tag enforces that
}

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	OwnerID     string    `json:"-"` // hidden from JSON; exposed via Owner resolver
	CreatedAt   time.Time `json:"createdAt"`
}

type Task struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"-"`
	Title       string     `json:"title"`
	Description *string    `json:"description,omitempty"`
	Status      TaskStatus `json:"status"`
	AssigneeID  *string    `json:"-"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

type AuthPayload struct {
	Token string `json:"token"`
	User  *User  `json:"user"`
}
