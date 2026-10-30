# docs/ — notes for Claude

The version notes in `docs/` are written for **people outside this repo**: the other lane owners and the coding agents they hand our docs to. They have not seen our code, so a note has to work without it.

| Lane | Owner | Usually needs from us |
|---|---|---|
| CI | Osman | push events (`RepositoryPushed`), file content and source archives at a commit |
| Dash | Bilge | repositories, branches, commits, trees, diffs and merges for the code browser and merge requests |
| Console | Beste | repository administration; auth-svc issues the owner IDs we store |
| Deploy, backup | Ceyda | config, ports, volumes, the gateway route for Git over HTTPS, what is in the backup |

## What goes where

| Path | What | Source of truth? |
|---|---|---|
| `../proto/git/v1/` | The gRPC API and events, every field documented | Yes |
| `version_notes/<year>/<month>/<day>_<month>.md` | What changed on one day and what each lane must do about it | No. It summarizes a change and links to the protos and ADRs |
| `version_notes/overview.md` | One line per note, newest first | Index |
| `decisions/` | ADRs | For us. Others may read them |
| `../migrations/` | Copy of our migrations | No: the git2 repo's `migrations/` is |

## When a change needs docs

A change needs docs when anyone outside this repo can notice it: a proto change (service, RPC, message, field, error code), an event, a config variable or default, a limit, behaviour another service relies on, a schema change, or a deploy requirement.

In the same PR:

1. Update the `.proto` comments (then `make proto`) so they describe the new state. History belongs in the version note.
2. Add today's version note, or extend it if it already exists.
3. Add a line at the top of the month's table in `version_notes/overview.md`, creating the month's section if needed.
4. If a lane must build something (a gateway route, a migration review, a deploy change), write the action items in the note so an issue can link to them. Don't open issues unless asked.

## Version notes

Path: `version_notes/<year>/<month>/<day>_<month>.md`, for example `version_notes/2026/10/26_10.md`. One note per day. A second change on the same day goes into the same note.

```markdown
# Codeplane version notes — 2026-10-26

**Audience:** <lanes this is for> and their coding agents.
**Scope:** <one or two sentences: what changed>

**Canonical reference:** [proto/git/v1/<file>.proto](../../../../proto/git/v1/<file>.proto).
The protos are the source of truth; this note only summarizes the change.

**Breaking?** <No / Yes: what stops working, and for whom>

---

## 🟢 NEW — <thing>          (or 🟡 CHANGED, 🔴 REMOVED, ℹ️ FYI)

<What it is, then an example: a grpcurl call or a Go snippet.
A table of error codes and when each happens. Before/after tables for changes.>

## Action items

### Deploy (Ceyda)
- [ ] <concrete, checkable step>
```

Only list the lanes that have something to do.

## Overview tags

| Tag | Meaning |
|---|---|
| 🔴 Breaking | Something a lane already built stops working until they change it. |
| 🟠 Action | Nothing breaks, but a lane must build or change something. |
| 🟢 New | New RPC, field, event or option. Use it when you're ready. |
| ⚪ FYI | Inside codeplane only. Nothing to do. |

## Style

- Plain English and short sentences. Say what the reader must do, not how we built it.
- Be concrete: real RPC, field and enum names, an example call, and when each error code happens.
- Name the lane each action item is for.
- Never paste secrets, DSNs, tokens or internal hostnames, even in examples. Use `<placeholder>`.
- Links are relative, so they work on GitHub and in a local checkout.
