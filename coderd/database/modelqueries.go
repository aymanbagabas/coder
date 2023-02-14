package database

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"golang.org/x/xerrors"

	"github.com/coder/coder/coderd/rbac"
	"github.com/coder/coder/coderd/rbac/regosql"
)

const (
	authorizedQueryPlaceholder = "-- @authorize_filter"
)

// customQuerier encompasses all non-generated queries.
// It provides a flexible way to write queries for cases
// where sqlc proves inadequate.
type customQuerier interface {
	templateQuerier
	workspaceQuerier
	userQuerier
}

type templateQuerier interface {
	GetAuthorizedTemplates(ctx context.Context, arg GetTemplatesWithFilterParams, prepared rbac.PreparedAuthorized) ([]Template, error)
	GetTemplateGroupRoles(ctx context.Context, id uuid.UUID) ([]TemplateGroup, error)
	GetTemplateUserRoles(ctx context.Context, id uuid.UUID) ([]TemplateUser, error)
}

func (q *sqlQuerier) GetAuthorizedTemplates(ctx context.Context, arg GetTemplatesWithFilterParams, prepared rbac.PreparedAuthorized) ([]Template, error) {
	authorizedFilter, err := prepared.CompileToSQL(ctx, regosql.ConvertConfig{
		VariableConverter: regosql.TemplateConverter(),
	})
	if err != nil {
		return nil, xerrors.Errorf("compile authorized filter: %w", err)
	}

	filtered, err := insertAuthorizedFilter(getTemplatesWithFilter, fmt.Sprintf(" AND %s", authorizedFilter))
	if err != nil {
		return nil, xerrors.Errorf("insert authorized filter: %w", err)
	}

	// The name comment is for metric tracking
	query := fmt.Sprintf("-- name: GetAuthorizedTemplates :many\n%s", filtered)
	rows, err := q.db.QueryContext(ctx, query,
		arg.Deleted,
		arg.OrganizationID,
		arg.ExactName,
		pq.Array(arg.IDs),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Template
	for rows.Next() {
		var i Template
		if err := rows.Scan(
			&i.ID,
			&i.CreatedAt,
			&i.UpdatedAt,
			&i.OrganizationID,
			&i.Deleted,
			&i.Name,
			&i.Provisioner,
			&i.ActiveVersionID,
			&i.Description,
			&i.DefaultTTL,
			&i.CreatedBy,
			&i.Icon,
			&i.UserACL,
			&i.GroupACL,
			&i.DisplayName,
			&i.AllowUserCancelWorkspaceJobs,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

type TemplateUser struct {
	User
	Actions Actions `db:"actions"`
}

func (q *sqlQuerier) GetTemplateUserRoles(ctx context.Context, id uuid.UUID) ([]TemplateUser, error) {
	const query = `
	SELECT
		perms.value as actions, users.*
	FROM
		users
	JOIN
		(
			SELECT
				*
			FROM
				jsonb_each_text(
					(
						SELECT
							templates.user_acl
						FROM
							templates
						WHERE
							id = $1
					)
				)
		) AS perms
	ON
		users.id::text = perms.key
	WHERE
		users.deleted = false
	AND
		users.status = 'active';
	`

	var tus []TemplateUser
	err := q.db.SelectContext(ctx, &tus, query, id.String())
	if err != nil {
		return nil, xerrors.Errorf("select user actions: %w", err)
	}

	return tus, nil
}

type TemplateGroup struct {
	Group
	Actions Actions `db:"actions"`
}

func (q *sqlQuerier) GetTemplateGroupRoles(ctx context.Context, id uuid.UUID) ([]TemplateGroup, error) {
	const query = `
	SELECT
		perms.value as actions, groups.*
	FROM
		groups
	JOIN
		(
			SELECT
				*
			FROM
				jsonb_each_text(
					(
						SELECT
							templates.group_acl
						FROM
							templates
						WHERE
							id = $1
					)
				)
		) AS perms
	ON
		groups.id::text = perms.key;
	`

	var tgs []TemplateGroup
	err := q.db.SelectContext(ctx, &tgs, query, id.String())
	if err != nil {
		return nil, xerrors.Errorf("select group roles: %w", err)
	}

	return tgs, nil
}

type workspaceQuerier interface {
	GetAuthorizedWorkspaces(ctx context.Context, arg GetWorkspacesParams, prepared rbac.PreparedAuthorized) ([]WorkspaceWithData, error)
}

// WorkspaceWithData includes information returned by the api for a workspace.
type WorkspaceWithData struct {
	Workspace

	// User related fields.
	OwnerUserName                  string
	LatestBuildInitiatorUsername   string
	LatestBuildTemplateVersionName string

	// These template fields are included in the response for a workspace.
	// This means if you can read a workspace, you can also read these limited
	// template fields as they are metadata of the workspace.
	TemplateName                         string
	TemplateIcon                         string
	TemplateDisplayName                  string
	TemplateAllowUserCancelWorkspaceJobs bool
	TemplateActiveVersionID              uuid.UUID

	LatestBuild    WorkspaceBuild
	LatestBuildJob ProvisionerJob

	// Count is the total number of workspaces applicable to the query.
	// This is used for pagination as the total number of returned workspaces
	// could be less than this number.
	Count int64 `db:"count" json:"count"`
}

// GetAuthorizedWorkspaces returns all workspaces that the user is authorized to access.
// This code is copied from `GetWorkspaces` and adds the authorized filter WHERE
// clause.
func (q *sqlQuerier) GetAuthorizedWorkspaces(ctx context.Context, arg GetWorkspacesParams, prepared rbac.PreparedAuthorized) ([]WorkspaceWithData, error) {
	authorizedFilter, err := prepared.CompileToSQL(ctx, rbac.ConfigWithoutACL())
	if err != nil {
		return nil, xerrors.Errorf("compile authorized filter: %w", err)
	}

	// In order to properly use ORDER BY, OFFSET, and LIMIT, we need to inject the
	// authorizedFilter between the end of the where clause and those statements.
	filtered, err := insertAuthorizedFilter(getWorkspaces, fmt.Sprintf(" AND %s", authorizedFilter))
	if err != nil {
		return nil, xerrors.Errorf("insert authorized filter: %w", err)
	}

	// The name comment is for metric tracking
	query := fmt.Sprintf("-- name: GetAuthorizedWorkspaces :many\n%s", filtered)
	rows, err := q.db.QueryContext(ctx, query,
		arg.Deleted,
		arg.Status,
		arg.OwnerID,
		arg.OwnerUsername,
		arg.TemplateName,
		pq.Array(arg.TemplateIds),
		pq.Array(arg.WorkspaceIds),
		arg.Name,
		arg.HasAgent,
		arg.AgentInactiveDisconnectTimeoutSeconds,
		arg.Offset,
		arg.Limit,
	)
	if err != nil {
		return nil, xerrors.Errorf("get authorized workspaces: %w", err)
	}
	// TODO: Switch to sqlx for row scan?
	defer rows.Close()
	var items []WorkspaceWithData
	for rows.Next() {
		var i WorkspaceWithData
		if err := rows.Scan(
			&i.ID,
			&i.CreatedAt,
			&i.UpdatedAt,
			&i.OwnerID,
			&i.OrganizationID,
			&i.TemplateID,
			&i.Deleted,
			&i.Name,
			&i.AutostartSchedule,
			&i.Ttl,
			&i.LastUsedAt,
			&i.TemplateName,
			&i.TemplateIcon,
			&i.TemplateDisplayName,
			&i.TemplateAllowUserCancelWorkspaceJobs,
			&i.TemplateActiveVersionID,
			&i.LatestBuild.ID,
			&i.LatestBuild.CreatedAt,
			&i.LatestBuild.UpdatedAt,
			&i.LatestBuild.TemplateVersionID,
			&i.LatestBuild.BuildNumber,
			&i.LatestBuild.Transition,
			&i.LatestBuild.JobID,
			&i.LatestBuild.InitiatorID,
			&i.LatestBuild.Reason,
			&i.LatestBuild.DailyCost,
			&i.LatestBuildJob.ID,
			&i.LatestBuildJob.CreatedAt,
			&i.LatestBuildJob.UpdatedAt,
			&i.LatestBuildJob.StartedAt,
			&i.LatestBuildJob.CompletedAt,
			&i.LatestBuildJob.CanceledAt,
			&i.LatestBuildJob.Error,
			&i.LatestBuildJob.OrganizationID,
			&i.LatestBuildJob.Provisioner,
			&i.LatestBuildJob.StorageMethod,
			&i.LatestBuildJob.Type,
			&i.LatestBuildJob.WorkerID,
			&i.LatestBuildJob.FileID,
			&i.LatestBuildJob.Tags,
			&i.Count,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

type userQuerier interface {
	GetAuthorizedUserCount(ctx context.Context, arg GetFilteredUserCountParams, prepared rbac.PreparedAuthorized) (int64, error)
}

func (q *sqlQuerier) GetAuthorizedUserCount(ctx context.Context, arg GetFilteredUserCountParams, prepared rbac.PreparedAuthorized) (int64, error) {
	authorizedFilter, err := prepared.CompileToSQL(ctx, rbac.ConfigWithoutACL())
	if err != nil {
		return -1, xerrors.Errorf("compile authorized filter: %w", err)
	}

	filtered, err := insertAuthorizedFilter(getFilteredUserCount, fmt.Sprintf(" AND %s", authorizedFilter))
	if err != nil {
		return -1, xerrors.Errorf("insert authorized filter: %w", err)
	}

	query := fmt.Sprintf("-- name: GetAuthorizedUserCount :one\n%s", filtered)
	row := q.db.QueryRowContext(ctx, query,
		arg.Search,
		pq.Array(arg.Status),
		pq.Array(arg.RbacRole),
	)
	var count int64
	err = row.Scan(&count)
	return count, err
}

func insertAuthorizedFilter(query string, replaceWith string) (string, error) {
	if !strings.Contains(query, authorizedQueryPlaceholder) {
		return "", xerrors.Errorf("query does not contain authorized replace string, this is not an authorized query")
	}
	filtered := strings.Replace(query, authorizedQueryPlaceholder, replaceWith, 1)
	return filtered, nil
}
