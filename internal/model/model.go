// Package model holds the domain types shared by the store, service and HTTP layers.
package model

import (
	"errors"
	"time"
)

// Sentinel errors the store returns and the HTTP layer maps to status codes.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// Role controls what an API token may do.
type Role string

// Roles: viewers read, editors also create services, SLOs and events.
const (
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleViewer || r == RoleEditor }

// CanWrite reports whether r may change data.
func (r Role) CanWrite() bool { return r == RoleEditor }

// Service is a system that owns one or more SLOs.
type Service struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Team        string    `json:"team"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// SLO is a reliability objective for a service, e.g. "availability >= 99.9% over 30 days".
type SLO struct {
	ID          int64     `json:"id"`
	ServiceID   int64     `json:"service_id"`
	ServiceName string    `json:"service"`
	Name        string    `json:"name"`
	Target      float64   `json:"target"`
	WindowDays  int       `json:"window_days"`
	CreatedAt   time.Time `json:"created_at"`
}

// Token is an API credential. Only a hash of the secret is ever stored.
type Token struct {
	ID        int64
	Name      string
	Role      Role
	CreatedAt time.Time
}

// ValidationError collects field-level problems with client input.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "validation failed" }

// Add records msg for field, keeping the first message if the field already has one.
func (e *ValidationError) Add(field, msg string) {
	if e.Fields == nil {
		e.Fields = map[string]string{}
	}
	if _, ok := e.Fields[field]; !ok {
		e.Fields[field] = msg
	}
}

// Err returns e if any field failed validation, otherwise nil.
func (e *ValidationError) Err() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}
