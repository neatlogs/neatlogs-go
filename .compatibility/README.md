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
4. optionally asks Gemini to review one uncovered, testable module and propose
   one small SDK source fix tied to a cited evidence path;
5. validates any proposed patch in a separate read-only job with no Gemini or
   write credentials, rerunning affected adapter suites at baseline and latest;
6. opens or reuses a draft fix PR for human review when the bounded patch
   passes validation, then updates the review issue and Slack;
7. alerts Slack when a release is
   newer than the analyzed version lock. The same releases can trigger another
   alert on later runs until the lock is updated after review.

The alert reports each module as pass, fail, blocked, or not tested. A fail means
the mapped adapter suite passed at the baseline and failed at the new version;
it is a regression in that tested scope. A newer upstream Go requirement is
blocked toolchain evidence, not an SDK test failure. Gemini can propose a fix
when concrete upstream and adapter evidence suggests a behavioral problem even
if mapped tests pass. In that case the draft PR explicitly says that the
suspected regression has not been reproduced and needs a focused test. A risk
score alone never opens a PR. Proposed edits are limited to two allowlisted Go
adapter source files and are applied by exact text replacement; workflow files,
tests, documentation, and manifests cannot be edited by the model. No automated
PR is merged. The review issue links the run, evidence, test results, Gemini
analysis, and any draft PR. Additional candidate findings remain for later
runs; an existing automated draft PR prevents duplicate proposals for its
release, including one closed by a reviewer.

Configure these GitHub Actions settings:

- Secret `COMPAT_GEMINI_API_KEY` (optional): a dedicated, quota-limited Gemini
  API key. Without it, deterministic discovery/evidence still runs and the LLM
  step records that it was skipped.
- Variable `COMPAT_GEMINI_MODEL` (optional): model override; defaults to
  `gemini-2.5-flash`.
- Secret `COMPAT_SLACK_WEBHOOK_URL` (optional): a channel-specific Slack
  Incoming Webhook. Without it, Slack notification is skipped.
- Secret `COMPAT_PR_TOKEN` (optional): a narrowly scoped GitHub App or PAT token
  with repository contents and pull-request write access. The publish job uses
  it only for branch and draft PR creation; without it, that step uses the
  workflow `GITHUB_TOKEN`.

Organization-level secrets scoped only to the SDK repositories are preferred.
The credentials are used only by the scheduled/default-branch workflow and are
never passed to pull-request jobs. Slack failures are non-blocking; alerts are
sent for releases newer than the analyzed lock or for workflow failures.
Gemini failures are recorded as an unavailable advisory and do not prevent
deterministic evidence, adapter tests, or the review issue.

The `publish` job needs repository Actions settings that allow GitHub Actions
to create pull requests, plus `contents: write` and `pull-requests: write`
permissions for its `GITHUB_TOKEN`, or the optional `COMPAT_PR_TOKEN` described
above. If repository policy blocks PR creation, the workflow reports the failed
publication in the issue, Slack, and run logs. Draft PRs made with the default
`GITHUB_TOKEN` may require human approval before pull-request checks run;
a GitHub App or PAT token can allow those checks to trigger automatically.
