// Package gql builds the GraphQL schema in Go code using graphql-go.
//
// Why graphql-go instead of gqlgen?
//   - No code generation step. Every type is visible Go code you can read
//     and step through. For a portfolio/learning project that clarity wins.
//   - Dependencies: one library, one import path.
//
// What we give up vs gqlgen:
//   - Less type safety: resolver args come in as map[string]interface{} and
//     we type-assert them. gqlgen would generate strongly-typed arg structs.
//   - More boilerplate: every GraphQL type is hand-wired.
//
// The README discusses when you'd switch to gqlgen (answer: once the schema
// gets past ~20 types or you want subscriptions over websockets out of the box).
package gql

import (
	"fmt"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"

	"taskflow/internal/model"
)

// Schema is the root GraphQL schema. Built once at startup by New().
type Schema struct {
	Schema   graphql.Schema
	Resolver *Resolver
}

// timeScalar is a custom scalar that serializes time.Time as RFC3339 strings.
// graphql-go doesn't ship a Time scalar, so we define our own. Frontend just
// sees ISO-8601 strings, which JavaScript's `new Date(...)` parses directly.
var timeScalar = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "Time",
	Description: "ISO-8601 timestamp",
	// Serialize runs when sending a value to the client.
	Serialize: func(v interface{}) interface{} {
		if t, ok := v.(time.Time); ok {
			return t.Format(time.RFC3339)
		}
		return nil
	},
	// ParseValue runs when a variable comes in from the client.
	ParseValue: func(v interface{}) interface{} {
		if s, ok := v.(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
		return nil
	},
	// ParseLiteral handles inline literals in a query string.
	// We return nil — clients should pass times as variables, not literals.
	ParseLiteral: func(ast.Value) interface{} { return nil },
})

// New builds the schema. Takes the resolver so field resolve funcs can
// close over it. The Schema value is safe for concurrent use — graphql-go
// does not mutate it once built.
func New(r *Resolver) (*Schema, error) {
	// Enum for TaskStatus. graphql-go rejects any value not in this list
	// at parse time, so invalid statuses never reach our resolvers.
	taskStatusEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "TaskStatus",
		Values: graphql.EnumValueConfigMap{
			"TODO":        {Value: model.StatusTodo},
			"IN_PROGRESS": {Value: model.StatusInProgress},
			"DONE":        {Value: model.StatusDone},
		},
	})

	// ----- User type -----
	// Fields are defined as a map from field name -> resolve config.
	// For simple fields we don't specify a Resolve func; graphql-go pulls
	// the value off the source object by name (User.ID -> "id", etc).
	userType := graphql.NewObject(graphql.ObjectConfig{
		Name: "User",
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"email":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(timeScalar)},
		},
	})

	// ----- Project type (forward-declared for cyclic references) -----
	// Task.project -> Project.tasks -> Task is a cycle. graphql-go resolves
	// this by letting us pass a function that builds the Fields map lazily.
	var projectType *graphql.Object
	var taskType *graphql.Object

	projectType = graphql.NewObject(graphql.ObjectConfig{
		Name: "Project",
		// Fields is a FieldsThunk — evaluated after both types exist.
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"description": &graphql.Field{Type: graphql.String},
				"createdAt":   &graphql.Field{Type: graphql.NewNonNull(timeScalar)},
				// Owner is loaded lazily so a client asking for just
				// `project { id name }` pays no cost to fetch the user row.
				"owner": &graphql.Field{
					Type:    graphql.NewNonNull(userType),
					Resolve: r.resolveProjectOwner,
				},
				"tasks": &graphql.Field{
					Type:    graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(taskType))),
					Resolve: r.resolveProjectTasks,
				},
			}
		}),
	})

	taskType = graphql.NewObject(graphql.ObjectConfig{
		Name: "Task",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"title":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"description": &graphql.Field{Type: graphql.String},
				"status":      &graphql.Field{Type: graphql.NewNonNull(taskStatusEnum)},
				"createdAt":   &graphql.Field{Type: graphql.NewNonNull(timeScalar)},
				"updatedAt":   &graphql.Field{Type: graphql.NewNonNull(timeScalar)},
				"project": &graphql.Field{
					Type:    graphql.NewNonNull(projectType),
					Resolve: r.resolveTaskProject,
				},
				// assignee is NULLABLE — no NewNonNull wrapper.
				// This matches the GraphQL schema and the DB column.
				"assignee": &graphql.Field{
					Type:    userType,
					Resolve: r.resolveTaskAssignee,
				},
			}
		}),
	})

	// AuthPayload: wraps the token + user after login/signup.
	authPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AuthPayload",
		Fields: graphql.Fields{
			"token": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"user":  &graphql.Field{Type: graphql.NewNonNull(userType)},
		},
	})

	// ----- Root Query -----
	rootQuery := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"me": &graphql.Field{
				Type:    userType, // nullable - null means not logged in
				Resolve: r.resolveMe,
			},
			"projects": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(projectType))),
				Resolve: r.resolveProjects,
			},
			"project": &graphql.Field{
				Type: projectType,
				Args: graphql.FieldConfigArgument{
					"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
				Resolve: r.resolveProject,
			},
			"task": &graphql.Field{
				Type: taskType,
				Args: graphql.FieldConfigArgument{
					"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
				Resolve: r.resolveTask,
			},
		},
	})

	// ----- Input types -----
	// Inputs are distinct from objects in GraphQL. Attempting to use an
	// InputObject where an Object is expected (or vice versa) is a schema error.
	signupInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "SignupInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"email":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"name":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"password": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	loginInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "LoginInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"email":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"password": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	createProjectInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateProjectInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"name":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"description": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	createTaskInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateTaskInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"projectId":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"description": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"assigneeId":  &graphql.InputObjectFieldConfig{Type: graphql.ID},
		},
	})
	updateTaskInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateTaskInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"id":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":       &graphql.InputObjectFieldConfig{Type: graphql.String},
			"description": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"status":      &graphql.InputObjectFieldConfig{Type: taskStatusEnum},
			"assigneeId":  &graphql.InputObjectFieldConfig{Type: graphql.ID},
		},
	})

	// ----- Root Mutation -----
	rootMutation := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"signup": &graphql.Field{
				Type: graphql.NewNonNull(authPayloadType),
				Args: graphql.FieldConfigArgument{
					"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(signupInput)},
				},
				Resolve: r.resolveSignup,
			},
			"login": &graphql.Field{
				Type: graphql.NewNonNull(authPayloadType),
				Args: graphql.FieldConfigArgument{
					"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(loginInput)},
				},
				Resolve: r.resolveLogin,
			},
			"createProject": &graphql.Field{
				Type: graphql.NewNonNull(projectType),
				Args: graphql.FieldConfigArgument{
					"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createProjectInput)},
				},
				Resolve: r.resolveCreateProject,
			},
			"createTask": &graphql.Field{
				Type: graphql.NewNonNull(taskType),
				Args: graphql.FieldConfigArgument{
					"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createTaskInput)},
				},
				Resolve: r.resolveCreateTask,
			},
			"updateTask": &graphql.Field{
				Type: graphql.NewNonNull(taskType),
				Args: graphql.FieldConfigArgument{
					"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateTaskInput)},
				},
				Resolve: r.resolveUpdateTask,
			},
			"deleteTask": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Args: graphql.FieldConfigArgument{
					"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
				Resolve: r.resolveDeleteTask,
			},
		},
	})

	// Assemble.
	schema, err := graphql.NewSchema(graphql.SchemaConfig{
		Query:    rootQuery,
		Mutation: rootMutation,
		// graphql-go has no built-in subscription transport. For real-time
		// updates the HTML frontend uses an SSE endpoint wired directly to
		// the pub/sub (see server.go: /api/events). See README for why.
	})
	if err != nil {
		return nil, fmt.Errorf("build schema: %w", err)
	}
	return &Schema{Schema: schema, Resolver: r}, nil
}
