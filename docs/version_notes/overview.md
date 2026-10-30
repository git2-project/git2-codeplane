# Version notes overview

One line per codeplane change that touches another lane, **newest first**.
Open the linked note for the details, and use this page to check what your
lane has and hasn't done yet.

| Tag | Meaning |
|---|---|
| 🔴 Breaking | Something your lane already built stops working until you change it. |
| 🟠 Action | Nothing breaks, but your lane must build or change something. |
| 🟢 New | New RPC, field, event or option. Use it when you're ready. |
| ⚪ FYI | Inside codeplane only. Nothing to do. |

**Affects** names the lanes with something to read or do: CI (Osman),
Dash (Bilge), Console (Beste), Deploy (Ceyda).

Notes live in `<year>/<month>/<day>_<month>.md`. When you add one, put it in
the right month folder and add a line at the top of that month's table below.

---

## 2026 · October

| Date | What changed | Affects | Tag |
|---|---|---|---|
| [26 Oct](2026/10/26_10.md) | git-svc's first gRPC contract, `git.v1`, is ready for review. `RepositoryService` creates, gets, finds, lists and deletes repositories, and every create and delete writes an event to `git_svc.outbox`. The `git_svc` schema comes as a migration PR on git2. Deploy: a gateway slug lookup and a volume for repositories. CI: answers to A-02–A-04 and A-12–A-14. | All | 🟠 Action |
