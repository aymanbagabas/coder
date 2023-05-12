// Package wsbuilder provides the Builder object, which encapsulates the common business logic of inserting a new
// workspace build into the database.
package wsbuilder

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/tabbed/pqtype"
	"golang.org/x/xerrors"

	"github.com/coder/coder/coderd/conversion"
	"github.com/coder/coder/coderd/database"
	"github.com/coder/coder/coderd/httpapi"
	"github.com/coder/coder/coderd/provisionerdserver"
	"github.com/coder/coder/coderd/rbac"
	"github.com/coder/coder/coderd/tracing"
	"github.com/coder/coder/codersdk"
)

// Builder encapsulates the business logic of inserting a new workspace build into the database.
//
// Builder follows the so-called "Builder" pattern where options that customize the kind of build you get return
// a new instance of the Builder with the option applied.
//
// Example:
//
// b = wsbuilder.New(workspace, transition).VersionID(vID).Initiator(me)
// build, job, err := b.Build(...)
type Builder struct {
	// settings that control the kind of build you get
	workspace             database.Workspace
	trans                 database.WorkspaceTransition
	version               versionTarget
	state                 stateTarget
	logLevel              string
	legacyParameterValues []codersdk.CreateParameterRequest
	richParameterValues   []codersdk.WorkspaceBuildParameter
	initiator             uuid.UUID
	reason                database.BuildReason

	// used during build, makes function arguments less verbose
	ctx   context.Context
	store database.Store

	// cache of objects, so we only fetch once
	template                  *database.Template
	templateVersion           *database.TemplateVersion
	templateVersionJob        *database.ProvisionerJob
	templateVersionParameters *[]database.TemplateVersionParameter
	lastBuild                 *database.WorkspaceBuild
	lastBuildParameters       *[]database.WorkspaceBuildParameter
	lastParameterValues       *[]database.ParameterValue
	lastBuildJob              *database.ProvisionerJob
}

type versionTarget struct {
	active   bool
	specific *uuid.UUID
}

type stateTarget struct {
	orphan   bool
	explicit *[]byte
}

func New(w database.Workspace, t database.WorkspaceTransition) Builder {
	return Builder{workspace: w, trans: t}
}

// Methods that customize the build are public, have a struct receiver and return a new Builder.

func (b Builder) VersionID(v uuid.UUID) Builder {
	b.version = versionTarget{specific: &v}
	return b
}

func (b Builder) ActiveVersion() Builder {
	b.version = versionTarget{active: true}
	return b
}

func (b Builder) State(state []byte) Builder {
	b.state = stateTarget{explicit: &state}
	return b
}

func (b Builder) Orphan() Builder {
	b.state = stateTarget{orphan: true}
	return b
}

func (b Builder) LogLevel(l string) Builder {
	b.logLevel = l
	return b
}

func (b Builder) Initiator(u uuid.UUID) Builder {
	b.initiator = u
	return b
}

func (b Builder) Reason(r database.BuildReason) Builder {
	b.reason = r
	return b
}

func (b Builder) LegacyParameterValues(p []codersdk.CreateParameterRequest) Builder {
	b.legacyParameterValues = p
	return b
}

func (b Builder) RichParameterValues(p []codersdk.WorkspaceBuildParameter) Builder {
	b.richParameterValues = p
	return b
}

// SetLastWorkspaceBuildInTx prepopulates the Builder's cache with the last workspace build.  This allows us
// to avoid a repeated database query when the Builder's caller also needs the workspace build, e.g. auto-start &
// auto-stop.
//
// CAUTION: only call this method from within a database transaction with RepeatableRead isolation.  This transaction
// MUST be the database.Store you call Build() with.
func (b Builder) SetLastWorkspaceBuildInTx(build *database.WorkspaceBuild) Builder {
	b.lastBuild = build
	return b
}

// SetLastWorkspaceBuildJobInTx prepopulates the Builder's cache with the last workspace build job.  This allows us
// to avoid a repeated database query when the Builder's caller also needs the workspace build job, e.g. auto-start &
// auto-stop.
//
// CAUTION: only call this method from within a database transaction with RepeatableRead isolation.  This transaction
// MUST be the database.Store you call Build() with.
func (b Builder) SetLastWorkspaceBuildJobInTx(job *database.ProvisionerJob) Builder {
	b.lastBuildJob = job
	return b
}

type BuildError struct {
	// Status is a suitable HTTP status code
	Status  int
	Message string
	Wrapped error
}

func (e BuildError) Error() string {
	return e.Wrapped.Error()
}

func (e BuildError) Unwrap() error {
	return e.Wrapped
}

// Build computes and inserts a new workspace build into the database.  If authFunc is provided, it also performs
// authorization preflight checks.
func (b *Builder) Build(
	ctx context.Context,
	store database.Store,
	authFunc func(action rbac.Action, object rbac.Objecter) bool,
) (
	*database.WorkspaceBuild, *database.ProvisionerJob, error,
) {
	b.ctx = ctx

	// Run the build in a transaction with RepeatableRead isolation, and retries.
	// RepeatableRead isolation ensures that we get a consistent view of the database while
	// computing the new build.  This simplifies the logic so that we do not need to worry if
	// later reads are consistent with earlier ones.
	var err error
	for retries := 0; retries < 5; retries++ {
		var workspaceBuild *database.WorkspaceBuild
		var provisionerJob *database.ProvisionerJob
		err := store.InTx(func(store database.Store) error {
			b.store = store
			workspaceBuild, provisionerJob, err = b.buildTx(authFunc)
			return err
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		var pqe *pq.Error
		if xerrors.As(err, &pqe) {
			if pqe.Code == "40001" {
				// serialization error, retry
				continue
			}
		}
		if err != nil {
			// Other (hard) error
			return nil, nil, err
		}
		return workspaceBuild, provisionerJob, nil
	}
	return nil, nil, xerrors.Errorf("too many errors; last error: %w", err)
}

// buildTx contains the business logic of computing a new build.  Attributes of the new database objects are computed
// in a functional style, rather than imperative, to emphasize the logic of how they are defined.  A simple cache
// of database-fetched objects is stored on the struct to ensure we only fetch things once, even if they are used in
// the calculation of multiple attributes.
//
// In order to utilize this cache, the functions that compute build attributes use a pointer receiver type.
func (b *Builder) buildTx(authFunc func(action rbac.Action, object rbac.Objecter) bool) (
	*database.WorkspaceBuild, *database.ProvisionerJob, error,
) {
	if authFunc != nil {
		err := b.authorize(authFunc)
		if err != nil {
			return nil, nil, err
		}
	}
	err := b.checkTemplateVersionMatchesTemplate()
	if err != nil {
		return nil, nil, err
	}
	err = b.checkTemplateJobStatus()
	if err != nil {
		return nil, nil, err
	}
	err = b.checkRunningBuild()
	if err != nil {
		return nil, nil, err
	}

	template, err := b.getTemplate()
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "failed to fetch template", err}
	}

	templateVersionJob, err := b.getTemplateVersionJob()
	if err != nil {
		return nil, nil, BuildError{
			http.StatusInternalServerError, "failed to fetch template version job", err,
		}
	}

	legacyParameters, err := b.getLastParameterValues()
	if err != nil {
		return nil, nil, BuildError{
			http.StatusInternalServerError,
			"failed to fetch previous legacy parameters.",
			err,
		}
	}

	// if we haven't been told specifically who initiated, default to owner
	if b.initiator == uuid.Nil {
		b.initiator = b.workspace.OwnerID
	}
	// default reason is initiator
	if b.reason == "" {
		b.reason = database.BuildReasonInitiator
	}

	// Write/Update any new params
	now := database.Now()
	for _, param := range b.legacyParameterValues {
		for _, exists := range legacyParameters {
			// If the param exists, delete the old param before inserting the new one
			if exists.Name == param.Name {
				err = b.store.DeleteParameterValueByID(b.ctx, exists.ID)
				if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
					return nil, nil, BuildError{
						http.StatusInternalServerError,
						fmt.Sprintf("Failed to delete old param %q", exists.Name),
						err,
					}
				}
			}
		}

		_, err = b.store.InsertParameterValue(b.ctx, database.InsertParameterValueParams{
			ID:                uuid.New(),
			Name:              param.Name,
			CreatedAt:         now,
			UpdatedAt:         now,
			Scope:             database.ParameterScopeWorkspace,
			ScopeID:           b.workspace.ID,
			SourceScheme:      database.ParameterSourceScheme(param.SourceScheme),
			SourceValue:       param.SourceValue,
			DestinationScheme: database.ParameterDestinationScheme(param.DestinationScheme),
		})
		if err != nil {
			return nil, nil, BuildError{http.StatusInternalServerError, "insert parameter value", err}
		}
	}

	workspaceBuildID := uuid.New()
	input, err := json.Marshal(provisionerdserver.WorkspaceProvisionJob{
		WorkspaceBuildID: workspaceBuildID,
		LogLevel:         b.logLevel,
	})
	if err != nil {
		return nil, nil, xerrors.Errorf("marshal provision job: %w", err)
	}
	traceMetadataRaw, err := json.Marshal(tracing.MetadataFromContext(b.ctx))
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "marshal metadata", err}
	}
	tags := provisionerdserver.MutateTags(b.workspace.OwnerID, templateVersionJob.Tags)

	provisionerJob, err := b.store.InsertProvisionerJob(b.ctx, database.InsertProvisionerJobParams{
		ID:             uuid.New(),
		CreatedAt:      database.Now(),
		UpdatedAt:      database.Now(),
		InitiatorID:    b.initiator,
		OrganizationID: template.OrganizationID,
		Provisioner:    template.Provisioner,
		Type:           database.ProvisionerJobTypeWorkspaceBuild,
		StorageMethod:  templateVersionJob.StorageMethod,
		FileID:         templateVersionJob.FileID,
		Input:          input,
		Tags:           tags,
		TraceMetadata: pqtype.NullRawMessage{
			Valid:      true,
			RawMessage: traceMetadataRaw,
		},
	})
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "insert provisioner job", err}
	}

	templateVersionID, err := b.getTemplateVersionID()
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "compute template version ID", err}
	}
	buildNum, err := b.getBuildNumber()
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "compute build number", err}
	}
	state, err := b.getState()
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "compute build state", err}
	}
	workspaceBuild, err := b.store.InsertWorkspaceBuild(b.ctx, database.InsertWorkspaceBuildParams{
		ID:                workspaceBuildID,
		CreatedAt:         database.Now(),
		UpdatedAt:         database.Now(),
		WorkspaceID:       b.workspace.ID,
		TemplateVersionID: templateVersionID,
		BuildNumber:       buildNum,
		ProvisionerState:  state,
		InitiatorID:       b.initiator,
		Transition:        b.trans,
		JobID:             provisionerJob.ID,
		Reason:            b.reason,
	})
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "insert workspace build", err}
	}

	names, values, err := b.getParameters()
	if err != nil {
		// getParameters already wraps errors in BuildError
		return nil, nil, err
	}
	err = b.store.InsertWorkspaceBuildParameters(b.ctx, database.InsertWorkspaceBuildParametersParams{
		WorkspaceBuildID: workspaceBuildID,
		Name:             names,
		Value:            values,
	})
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "insert workspace build parameters: %w", err}
	}

	return &workspaceBuild, &provisionerJob, nil
}

func (b *Builder) getTemplate() (*database.Template, error) {
	if b.template != nil {
		return b.template, nil
	}
	t, err := b.store.GetTemplateByID(b.ctx, b.workspace.TemplateID)
	if err != nil {
		return nil, err
	}
	b.template = &t
	return b.template, nil
}

func (b *Builder) getTemplateVersionJob() (*database.ProvisionerJob, error) {
	if b.templateVersionJob != nil {
		return b.templateVersionJob, nil
	}
	v, err := b.getTemplateVersion()
	if err != nil {
		return nil, err
	}
	j, err := b.store.GetProvisionerJobByID(b.ctx, v.JobID)
	if err != nil {
		return nil, err
	}
	b.templateVersionJob = &j
	return b.templateVersionJob, err
}

func (b *Builder) getTemplateVersion() (*database.TemplateVersion, error) {
	if b.templateVersion != nil {
		return b.templateVersion, nil
	}
	id, err := b.getTemplateVersionID()
	if err != nil {
		return nil, err
	}
	v, err := b.store.GetTemplateVersionByID(b.ctx, id)
	if err != nil {
		return nil, err
	}
	b.templateVersion = &v
	return b.templateVersion, err
}

func (b *Builder) getTemplateVersionID() (uuid.UUID, error) {
	if b.version.specific != nil {
		return *b.version.specific, nil
	}
	if b.version.active {
		t, err := b.getTemplate()
		if err != nil {
			return uuid.Nil, err
		}
		return t.ActiveVersionID, nil
	}
	// default is prior version
	bld, err := b.getLastBuild()
	if err != nil {
		return uuid.Nil, err
	}
	return bld.TemplateVersionID, nil
}

func (b *Builder) getLastBuild() (*database.WorkspaceBuild, error) {
	if b.lastBuild != nil {
		return b.lastBuild, nil
	}
	bld, err := b.store.GetLatestWorkspaceBuildByWorkspaceID(b.ctx, b.workspace.ID)
	if err != nil {
		return nil, err
	}
	b.lastBuild = &bld
	return b.lastBuild, nil
}

func (b *Builder) getBuildNumber() (int32, error) {
	bld, err := b.getLastBuild()
	if err != nil {
		return 0, err
	}
	return bld.BuildNumber + 1, nil
}

func (b *Builder) getState() ([]byte, error) {
	if b.state.orphan {
		// Orphan means empty state.
		return nil, nil
	}
	if b.state.explicit != nil {
		return *b.state.explicit, nil
	}
	// Default is to use state from prior build
	bld, err := b.getLastBuild()
	if err != nil {
		return nil, err
	}
	return bld.ProvisionerState, nil
}

func (b *Builder) getParameters() (names, values []string, err error) {
	templateVersionParameters, err := b.getTemplateVersionParameters()
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "failed to fetch template version parameters", err}
	}
	lastBuildParameters, err := b.getLastBuildParameters()
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "failed to fetch last build parameters", err}
	}
	lastParameterValues, err := b.getLastParameterValues()
	if err != nil {
		return nil, nil, BuildError{http.StatusInternalServerError, "failed to fetch last parameter values", err}
	}
	resolver := codersdk.ParameterResolver{
		Rich:   conversion.WorkspaceBuildParameters(lastBuildParameters),
		Legacy: conversion.Parameters(lastParameterValues),
	}
	for _, templateVersionParameter := range templateVersionParameters {
		tvp, err := conversion.TemplateVersionParameter(templateVersionParameter)
		if err != nil {
			return nil, nil, BuildError{http.StatusInternalServerError, "failed to convert template version parameter", err}
		}
		value, err := resolver.ValidateResolve(
			tvp,
			b.findNewBuildParameterValue(templateVersionParameter.Name),
		)
		if err != nil {
			// At this point, we've queried all the data we need from the database,
			// so the only errors are problems with the request (missing data, failed
			// validation, immutable parameters, etc.)
			return nil, nil, BuildError{http.StatusBadRequest, err.Error(), err}
		}
		names = append(names, templateVersionParameter.Name)
		values = append(values, value)
	}
	return names, values, nil
}

func (b *Builder) findNewBuildParameterValue(name string) *codersdk.WorkspaceBuildParameter {
	for _, v := range b.richParameterValues {
		if v.Name == name {
			return &v
		}
	}
	return nil
}

func (b *Builder) getLastBuildParameters() ([]database.WorkspaceBuildParameter, error) {
	if b.lastBuildParameters != nil {
		return *b.lastBuildParameters, nil
	}
	bld, err := b.getLastBuild()
	if err != nil {
		return nil, err
	}
	values, err := b.store.GetWorkspaceBuildParameters(b.ctx, bld.ID)
	if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return values, nil
}

func (b *Builder) getTemplateVersionParameters() ([]database.TemplateVersionParameter, error) {
	if b.templateVersionParameters != nil {
		return *b.templateVersionParameters, nil
	}
	tvID, err := b.getTemplateVersionID()
	if err != nil {
		return nil, err
	}
	tvp, err := b.store.GetTemplateVersionParameters(b.ctx, tvID)
	if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	b.templateVersionParameters = &tvp
	return tvp, nil
}

func (b *Builder) getLastParameterValues() ([]database.ParameterValue, error) {
	if b.lastParameterValues != nil {
		return *b.lastParameterValues, nil
	}
	pv, err := b.store.ParameterValues(b.ctx, database.ParameterValuesParams{
		Scopes:   []database.ParameterScope{database.ParameterScopeWorkspace},
		ScopeIds: []uuid.UUID{b.workspace.ID},
	})
	if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	b.lastParameterValues = &pv
	return pv, nil
}

func (b *Builder) getLastBuildJob() (*database.ProvisionerJob, error) {
	if b.lastBuildJob != nil {
		return b.lastBuildJob, nil
	}
	bld, err := b.getLastBuild()
	if err != nil {
		return nil, err
	}
	job, err := b.store.GetProvisionerJobByID(b.ctx, bld.JobID)
	if err != nil {
		return nil, err
	}
	b.lastBuildJob = &job
	return b.lastBuildJob, nil
}

// authorize performs build authorization pre-checks using the provided authFunc
func (b *Builder) authorize(authFunc func(action rbac.Action, object rbac.Objecter) bool) error {
	// Doing this up front saves a lot of work if the user doesn't have permission.
	// This is checked again in the dbauthz layer, but the check is cached
	// and will be a noop later.
	var action rbac.Action
	switch b.trans {
	case database.WorkspaceTransitionDelete:
		action = rbac.ActionDelete
	case database.WorkspaceTransitionStart, database.WorkspaceTransitionStop:
		action = rbac.ActionUpdate
	default:
		return BuildError{http.StatusBadRequest, fmt.Sprintf("Transition %q not supported.", b.trans), xerrors.New("")}
	}
	if !authFunc(action, b.workspace) {
		// We use the same wording as the httpapi to avoid leaking the existence of the workspace
		return BuildError{http.StatusNotFound, httpapi.ResourceNotFoundResponse.Message, xerrors.New("")}
	}

	template, err := b.getTemplate()
	if err != nil {
		return BuildError{http.StatusInternalServerError, "failed to fetch template", err}
	}

	// If custom state, deny request since user could be corrupting or leaking
	// cloud state.
	if b.state.explicit != nil || b.state.orphan {
		if !authFunc(rbac.ActionUpdate, template.RBACObject()) {
			return BuildError{http.StatusForbidden, "Only template managers may provide custom state", xerrors.New("")}
		}
	}

	if b.logLevel != "" && !authFunc(rbac.ActionUpdate, template) {
		return BuildError{
			http.StatusBadRequest,
			"Workspace builds with a custom log level are restricted to template authors only.",
			xerrors.New(""),
		}
	}
	return nil
}

func (b *Builder) checkTemplateVersionMatchesTemplate() error {
	template, err := b.getTemplate()
	if err != nil {
		return BuildError{http.StatusInternalServerError, "failed to fetch template", err}
	}
	templateVersion, err := b.getTemplateVersion()
	if err != nil {
		return BuildError{http.StatusInternalServerError, "failed to fetch template version", err}
	}
	if !templateVersion.TemplateID.Valid || templateVersion.TemplateID.UUID != template.ID {
		return BuildError{
			http.StatusBadRequest,
			"template version doesn't match template",
			xerrors.Errorf("templateVersion.TemplateID = %s, template.ID = %s",
				templateVersion.TemplateID, template.ID),
		}
	}
	return nil
}

func (b *Builder) checkTemplateJobStatus() error {
	templateVersion, err := b.getTemplateVersion()
	if err != nil {
		return BuildError{http.StatusInternalServerError, "failed to fetch template version", err}
	}

	templateVersionJob, err := b.getTemplateVersionJob()
	if err != nil {
		return BuildError{
			http.StatusInternalServerError, "failed to fetch template version job", err,
		}
	}

	templateVersionJobStatus := conversion.ConvertProvisionerJobStatus(*templateVersionJob)
	switch templateVersionJobStatus {
	case codersdk.ProvisionerJobPending, codersdk.ProvisionerJobRunning:
		return BuildError{
			http.StatusNotAcceptable,
			fmt.Sprintf("The provided template version is %s. Wait for it to complete importing!", templateVersionJobStatus),
			xerrors.New(""),
		}
	case codersdk.ProvisionerJobFailed:
		return BuildError{
			http.StatusBadRequest,
			fmt.Sprintf("The provided template version %q has failed to import: %q. You cannot build workspaces with it!", templateVersion.Name, templateVersionJob.Error.String),
			xerrors.New(""),
		}
	case codersdk.ProvisionerJobCanceled:
		return BuildError{
			http.StatusBadRequest,
			"The provided template version was canceled during import. You cannot build workspaces with it!",
			xerrors.New(""),
		}
	}
	return nil
}

func (b *Builder) checkRunningBuild() error {
	job, err := b.getLastBuildJob()
	if err != nil {
		return BuildError{http.StatusInternalServerError, "failed to fetch prior build", err}
	}
	if conversion.ConvertProvisionerJobStatus(*job).Active() {
		return BuildError{http.StatusConflict,
			"A workspace build is already active.",
			xerrors.New(""),
		}
	}
	return nil
}
