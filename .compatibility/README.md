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
3. runs the affected adapter package tests against the recorded baseline and
   the newly published module version in isolated temporary module files;
4. optionally asks Gemini for a separate advisory impact assessment;
5. updates a GitHub review issue and optionally alerts Slack when a release is
   newer than the analyzed version lock. The same releases can trigger another
   alert on later runs until the lock is updated after review.

The alert reports each module as pass, fail, blocked, or not tested. A fail means
the mapped adapter suite passed at the baseline and failed at the new version;
it is a regression in that tested scope. A newer upstream Go requirement is
blocked toolchain evidence, not an SDK test failure. The workflow does not
create a fix pull request. The review issue links the run and its evidence,
test results, and Gemini analysis. Gemini remains advisory and unverified.

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
deterministic evidence, adapter tests, or the review issue.
