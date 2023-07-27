import Link from "@mui/material/Link"
import { Workspace } from "api/typesGenerated"
import { Maybe } from "components/Conditionals/Maybe"
import { PaginationWidgetBase } from "components/PaginationWidget/PaginationWidgetBase"
import { ComponentProps, FC } from "react"
import { Link as RouterLink } from "react-router-dom"
import { Margins } from "components/Margins/Margins"
import {
  PageHeader,
  PageHeaderSubtitle,
  PageHeaderTitle,
} from "components/PageHeader/PageHeader"
import { Stack } from "components/Stack/Stack"
import { WorkspaceHelpTooltip } from "components/Tooltips"
import { WorkspacesTable } from "components/WorkspacesTable/WorkspacesTable"
import { useLocalStorage } from "hooks"
import { LockedWorkspaceBanner, Count } from "components/WorkspaceDeletion"
import { ErrorAlert } from "components/Alert/ErrorAlert"
import { WorkspacesFilter } from "./filter/filter"
import { hasError, isApiValidationError } from "api/errors"
import { PaginationStatus } from "components/PaginationStatus/PaginationStatus"

export const Language = {
  pageTitle: "Workspaces",
  yourWorkspacesButton: "Your workspaces",
  allWorkspacesButton: "All workspaces",
  runningWorkspacesButton: "Running workspaces",
  createANewWorkspace: `Create a new workspace from a `,
  template: "Template",
}

export interface WorkspacesPageViewProps {
  error: unknown
  workspaces?: Workspace[]
  count?: number
  filterProps: ComponentProps<typeof WorkspacesFilter>
  page: number
  limit: number
  onPageChange: (page: number) => void
  onUpdateWorkspace: (workspace: Workspace) => void
}

export const WorkspacesPageView: FC<
  React.PropsWithChildren<WorkspacesPageViewProps>
> = ({
  workspaces,
  error,
  limit,
  count,
  filterProps,
  onPageChange,
  onUpdateWorkspace,
  page,
}) => {
  const { saveLocal } = useLocalStorage()

  const workspacesDeletionScheduled = workspaces
    ?.filter((workspace) => workspace.deleting_at)
    .map((workspace) => workspace.id)

  const hasLockedWorkspace =
    workspaces?.find((workspace) => workspace.locked_at) !== undefined

  const hasLockedFilter = (): boolean => {
    for (const key in filterProps.filter.values) {
      if (key === "locked_at") {
        return true
      }
    }
    return false
  }

  const shownWorkspaces = !hasLockedFilter()
    ? workspaces?.filter((workspace) => !workspace.locked_at)
    : workspaces

  return (
    <Margins>
      <PageHeader>
        <PageHeaderTitle>
          <Stack direction="row" spacing={1} alignItems="center">
            <span>{Language.pageTitle}</span>
            <WorkspaceHelpTooltip />
          </Stack>
        </PageHeaderTitle>

        <PageHeaderSubtitle>
          {Language.createANewWorkspace}
          <Link component={RouterLink} to="/templates">
            {Language.template}
          </Link>
          .
        </PageHeaderSubtitle>
      </PageHeader>

      <Stack>
        <Maybe condition={hasError(error) && !isApiValidationError(error)}>
          <ErrorAlert error={error} />
        </Maybe>
        {/* <ImpendingDeletionBanner/> determines its own visibility */}
        <LockedWorkspaceBanner
          workspaces={workspaces}
          shouldRedisplayBanner={hasLockedWorkspace}
          onDismiss={() =>
            saveLocal(
              "dismissedWorkspaceList",
              JSON.stringify(workspacesDeletionScheduled),
            )
          }
          count={Count.Multiple}
        />

        <WorkspacesFilter error={error} {...filterProps} />
      </Stack>

      <PaginationStatus
        isLoading={!workspaces && !error}
        showing={shownWorkspaces?.length ?? 0}
        total={count ?? 0}
        label="workspaces"
      />

      <WorkspacesTable
        workspaces={shownWorkspaces}
        isUsingFilter={filterProps.filter.used}
        onUpdateWorkspace={onUpdateWorkspace}
        error={error}
      />
      {count !== undefined && (
        <PaginationWidgetBase
          count={count}
          limit={limit}
          onChange={onPageChange}
          page={page}
        />
      )}
    </Margins>
  )
}
