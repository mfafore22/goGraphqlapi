package gql

import (
	"context"
	"errors"
	"strings"

	"github.com/graphql-go/graphql"

	"taskflow/internal/auth"
	"taskflow/internal/model"
	"taskflow/internal/store"
)

// Resolver bundles the dependencies every resolver function needs.
// Fields are exported because main.go constructs this directly.
type Resolver struct {
	Store  *store.Store
	PubSub *TaskPubSub
}

func NewResolver(s *store.Store, ps *TaskPubSub) *Resolver {
	return &Resolver{Store: s, PubSub: ps}
}

// ---------- helpers ----------

// strArg pulls an optional string from resolver args map. graphql-go hands
// us interface{} values, so every field access requires a type assertion.
func strArg(m map[string]interface{}, k string) string {
	if v, ok := m[k]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// strPtrArg returns a *string so we can distinguish "not provided" from
// "empty string". Critical for PATCH-style updates.
func strPtrArg(m map[string]interface{}, k string) *string {
	v, ok := m[k]
	if !ok || v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

// requireUser pulls the user ID out of context or returns an auth error.
// Resolvers that require login call this as their first step.
func requireUser(ctx context.Context) (string, error) {
	uid := auth.UserFromContext(ctx)
	if uid == "" {
		return "", auth.ErrUnauthenticated
	}
	return uid, nil
}

// ---------- query resolvers ----------

func (r *Resolver) resolveMe(p graphql.ResolveParams) (interface{}, error) {
	uid := auth.UserFromContext(p.Context)
	if uid == "" {
		// Returning (nil, nil) makes the field null without an error.
		// This is the conventional way to express "not logged in" via `me`.
		return nil, nil
	}
	u, err := r.Store.UserByID(p.Context, uid)
	if err != nil {
		return nil, nil // stale token referencing a deleted user
	}
	return u, nil
}

func (r *Resolver) resolveProjects(p graphql.ResolveParams) (interface{}, error) {
	uid, err := requireUser(p.Context)
	if err != nil {
		return nil, err
	}
	return r.Store.ProjectsByOwner(p.Context, uid)
}

func (r *Resolver) resolveProject(p graphql.ResolveParams) (interface{}, error) {
	uid, err := requireUser(p.Context)
	if err != nil {
		return nil, err
	}
	id := strArg(p.Args, "id")
	proj, err := r.Store.ProjectByID(p.Context, id)
	if err != nil {
		return nil, errors.New("project not found")
	}
	// Authz check here in the resolver, not in SQL. Tradeoff: an extra in-
	// memory compare after the fetch. Benefit: uniform error message whether
	// the project doesn't exist or belongs to someone else — doesn't leak
	// whether a given ID corresponds to a real project.
	if proj.OwnerID != uid {
		return nil, errors.New("project not found")
	}
	return proj, nil
}

func (r *Resolver) resolveTask(p graphql.ResolveParams) (interface{}, error) {
	uid, err := requireUser(p.Context)
	if err != nil {
		return nil, err
	}
	id := strArg(p.Args, "id")
	t, err := r.Store.TaskByID(p.Context, id)
	if err != nil {
		return nil, errors.New("task not found")
	}
	// Auth: user must own the project this task belongs to.
	proj, err := r.Store.ProjectByID(p.Context, t.ProjectID)
	if err != nil || proj.OwnerID != uid {
		return nil, errors.New("task not found")
	}
	return t, nil
}

// ---------- field resolvers (nested types) ----------

// resolveProjectOwner is called when a query asks for Project.owner.
// p.Source is the parent Project. This is the N+1 hot spot — if a query asks
// for 100 projects' owners, we'll issue 100 user lookups. A dataloader would
// batch these; see README "Scaling notes".
func (r *Resolver) resolveProjectOwner(p graphql.ResolveParams) (interface{}, error) {
	proj, ok := p.Source.(*model.Project)
	if !ok {
		return nil, errors.New("invalid parent for owner")
	}
	return r.Store.UserByID(p.Context, proj.OwnerID)
}

func (r *Resolver) resolveProjectTasks(p graphql.ResolveParams) (interface{}, error) {
	proj, ok := p.Source.(*model.Project)
	if !ok {
		return nil, errors.New("invalid parent for tasks")
	}
	return r.Store.TasksByProject(p.Context, proj.ID)
}

func (r *Resolver) resolveTaskProject(p graphql.ResolveParams) (interface{}, error) {
	t, ok := p.Source.(*model.Task)
	if !ok {
		return nil, errors.New("invalid parent for project")
	}
	return r.Store.ProjectByID(p.Context, t.ProjectID)
}

func (r *Resolver) resolveTaskAssignee(p graphql.ResolveParams) (interface{}, error) {
	t, ok := p.Source.(*model.Task)
	if !ok {
		return nil, errors.New("invalid parent for assignee")
	}
	if t.AssigneeID == nil {
		return nil, nil // GraphQL null - unassigned is legal
	}
	u, err := r.Store.UserByID(p.Context, *t.AssigneeID)
	if err != nil {
		return nil, nil // assignee deleted; soft null rather than hard error
	}
	return u, nil
}

// ---------- mutation resolvers ----------

// inputMap extracts the nested "input" argument. Every mutation in our schema
// takes `input: SomeInput!`, so we centralize the extraction here.
func inputMap(p graphql.ResolveParams) (map[string]interface{}, error) {
	in, ok := p.Args["input"].(map[string]interface{})
	if !ok {
		return nil, errors.New("missing input")
	}
	return in, nil
}

func (r *Resolver) resolveSignup(p graphql.ResolveParams) (interface{}, error) {
	in, err := inputMap(p)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(strArg(in, "email")))
	name := strings.TrimSpace(strArg(in, "name"))
	password := strArg(in, "password")

	// Validation. GraphQL only enforces presence/type; semantic checks
	// (min length, valid email shape) are our responsibility.
	if !strings.Contains(email, "@") {
		return nil, errors.New("invalid email")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	if name == "" {
		return nil, errors.New("name required")
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	u, err := r.Store.CreateUser(p.Context, email, name, hash)
	if err != nil {
		return nil, err
	}
	token, err := auth.IssueToken(u.ID)
	if err != nil {
		return nil, err
	}
	return &model.AuthPayload{Token: token, User: u}, nil
}

func (r *Resolver) resolveLogin(p graphql.ResolveParams) (interface{}, error) {
	in, err := inputMap(p)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(strArg(in, "email")))
	password := strArg(in, "password")

	u, err := r.Store.UserByEmail(p.Context, email)
	if err != nil {
		// SAME error whether email missing or password wrong, to avoid
		// leaking which emails are registered (user enumeration protection).
		return nil, errors.New("invalid credentials")
	}
	if err := auth.CheckPassword(u.PasswordHash, password); err != nil {
		return nil, errors.New("invalid credentials")
	}
	token, err := auth.IssueToken(u.ID)
	if err != nil {
		return nil, err
	}
	return &model.AuthPayload{Token: token, User: u}, nil
}

func (r *Resolver) resolveCreateProject(p graphql.ResolveParams) (interface{}, error) {
	uid, err := requireUser(p.Context)
	if err != nil {
		return nil, err
	}
	in, err := inputMap(p)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(strArg(in, "name"))
	if name == "" {
		return nil, errors.New("name required")
	}
	return r.Store.CreateProject(p.Context, uid, name, strPtrArg(in, "description"))
}

func (r *Resolver) resolveCreateTask(p graphql.ResolveParams) (interface{}, error) {
	uid, err := requireUser(p.Context)
	if err != nil {
		return nil, err
	}
	in, err := inputMap(p)
	if err != nil {
		return nil, err
	}
	projectID := strArg(in, "projectId")
	title := strings.TrimSpace(strArg(in, "title"))
	if title == "" {
		return nil, errors.New("title required")
	}

	// Authz: project must belong to caller.
	proj, err := r.Store.ProjectByID(p.Context, projectID)
	if err != nil {
		return nil, errors.New("project not found")
	}
	if proj.OwnerID != uid {
		return nil, errors.New("forbidden")
	}

	t, err := r.Store.CreateTask(p.Context, projectID, title,
		strPtrArg(in, "description"), strPtrArg(in, "assigneeId"))
	if err != nil {
		return nil, err
	}
	// Publish AFTER successful persist — never broadcast a task that failed
	// to save. Otherwise subscribers see a task that then "vanishes" on reload.
	r.PubSub.Publish(projectID, t)
	return t, nil
}

func (r *Resolver) resolveUpdateTask(p graphql.ResolveParams) (interface{}, error) {
	uid, err := requireUser(p.Context)
	if err != nil {
		return nil, err
	}
	in, err := inputMap(p)
	if err != nil {
		return nil, err
	}

	taskID := strArg(in, "id")
	t, err := r.Store.TaskByID(p.Context, taskID)
	if err != nil {
		return nil, errors.New("task not found")
	}
	proj, err := r.Store.ProjectByID(p.Context, t.ProjectID)
	if err != nil || proj.OwnerID != uid {
		return nil, errors.New("forbidden")
	}

	// PATCH semantics: only update fields that were actually provided.
	// Presence in the args map means "client sent this field".
	if v, ok := in["title"]; ok && v != nil {
		t.Title = v.(string)
	}
	if v, ok := in["description"]; ok {
		// Allowed to be null — this sets description back to nothing.
		if v == nil {
			t.Description = nil
		} else {
			s := v.(string)
			t.Description = &s
		}
	}
	if v, ok := in["status"]; ok && v != nil {
		// Enum values arrive as the underlying Go value we registered
		// (model.TaskStatus), thanks to the enum config in schema.go.
		s, ok := v.(model.TaskStatus)
		if !ok {
			return nil, errors.New("invalid status")
		}
		t.Status = s
	}
	if v, ok := in["assigneeId"]; ok {
		if v == nil {
			t.AssigneeID = nil
		} else {
			s := v.(string)
			t.AssigneeID = &s
		}
	}

	if err := r.Store.UpdateTask(p.Context, t); err != nil {
		return nil, err
	}
	r.PubSub.Publish(t.ProjectID, t)
	return t, nil
}

func (r *Resolver) resolveDeleteTask(p graphql.ResolveParams) (interface{}, error) {
	uid, err := requireUser(p.Context)
	if err != nil {
		return nil, err
	}
	id := strArg(p.Args, "id")
	// Single-statement check+delete inside the store avoids any TOCTOU
	// race where ownership might change between a read and a delete.
	return r.Store.DeleteTaskByOwner(p.Context, id, uid)
}
