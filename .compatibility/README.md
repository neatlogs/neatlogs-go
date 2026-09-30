# Compatibility automation

This directory defines the integrations, package-manager matrix, supported
versions, and cross-integration contracts exercised by the compatibility
workflows.

## Scope source of truth

The inventory is limited to integrations documented for the Go SDK in
`neatlogs-docs`: explicit core helpers, Google GenAI, Google ADK, and the A2A
propagation helpers documented under Google ADK. Unsupported provider clients
and coding agents owned by separate repositories are intentionally excluded.

These workflows analyze real published module contents, exported APIs,
dependency graphs, changed source excerpts, the relevant adapter source, and
the official project documentation URLs declared for every integration.
Documentation fetch failures are retained as evidence gaps. They never initialize Neatlogs, call a
live model provider, export traces, or query a Neatlogs backend.

## Pull requests

The pull-request workflow is deterministic and does not receive external
service credentials. It creates an isolated consumer of the real SDK and
verifies module, workspace, vendor, and read-only module-resolution modes.

## Scheduled release monitoring

Twice a day (at 00:47 and 12:47 UTC), the scheduled workflow:

1. compares the analyzed version lock with the Go module proxy;
2. records dependency, exported API, source-content, adapter-source, and
   official project-documentation evidence;
3. optionally asks Gemini for an advisory impact assessment;
4. updates a GitHub review issue and optionally alerts Slack when a release is
   newer than the analyzed version lock. The same releases can trigger another
   alert on later runs until the lock is updated after review.

The alert means that review is needed, not that an SDK regression was found.
The workflow does not run the SDK against the new upstream versions or create a
fix pull request. A newer upstream Go requirement is recorded as deterministic
toolchain evidence even when it exceeds the CI runner's Go version. The review
issue links the workflow's evidence artifacts and Gemini analysis. The Gemini
assessment is advisory and unverified; verify any suspected breakage with
deterministic tests before updating the lock.

Configure these GitHub Actions settings:

- Secret `COMPAT_GEMINI_API_KEY` (optional): a dedicated, quota-limited Gemini
  API key. Without it, deterministic discovery/evidence still runs and the LLM
  step records that it was skipped.
- Variable `COMPAT_GEMINI_MODEL` (optional): model override; defaults to
  `gemini-2.5-flash`.
- Secret `COMPAT_SLACK_WEBHOOK_URL` (optional): a channel-specific Slack
  Incoming Webhook. Without it, Slack notification is skipped.

Organization-level secrets scoped only to the SDK repositories are preferred.
The credentials are used only by the scheduled/default-branch workflow and are
never passed to pull-request jobs. Slack failures are non-blocking; alerts are
sent for releases newer than the analyzed lock or for workflow failures.
Gemini failures are recorded as an unavailable advisory and do not prevent
deterministic evidence or the review issue.
