# CLAUDE.md — GIFsmith

Local video-to-GIF converter with local-only captioning and transcription. No Foreman/hard
hooks here (that's the latent-affect-audio-* projects) — this is a standard repo.

## Work is tracked in TESSERA

Ticket prefix: `GIF`, tracked in TESSERA (`/Users/m5/dev/ticket-system`, CLI: `python3 -m
tessera.api.cli --db data/tessera.db ...`, every write needs `--actor`). Log real work here —
create a ticket, freeze criteria before implementing, claim with `--file`/`--commit` evidence,
close once verified/rolled out.

## Standing rule: fail-closed for privacy/anonymity guarantees

See the global standing rule in `/Users/m5/.claude/CLAUDE.md` ("Fail-closed for any
privacy/anonymity/redaction guarantee"). This project's own worked example — the metadata-scrub
redesign — and the domain research behind it live at
`docs/domain/fail-closed-privacy-design.md`. Any future feature making a similar claim
("nothing traces back," "this is anonymized," "this is redacted") needs the same treatment:
write-temp → independently verify from disk → atomic publish → hard-fail on any error, not
fail-open with a hidden log line.

## Key/mode-adjacent gotcha for this project

Go type-checks every `.go` file under any directory in the module by default. Files moved to
`audible-deletion-flag/` (per the operator's global file-deletion convention — move, don't
delete, don't rename with an underscore) need a `//go:build ignore` tag or they silently break
`go build ./...`/`go vet ./...` for code nothing actually imports.

## Real gotchas

- `outPath`/job files live entirely in a per-job, server-generated, `0700`-permission temp
  directory (`jobs.go`) — never in a user-chosen folder during processing, so same-directory
  `os.Rename` is always same-filesystem (atomic) and there's no cloud-sync-folder edge case to
  worry about for this specific architecture.
- The project's threat model (documented in `docs/CVE-AUDIT.md`) is a malicious webpage
  exploiting the deliberately-permissive loopback CORS policy — not a co-resident malicious
  local process and not forensic recovery from the operator's own disk. Scope security/privacy
  work to that model unless explicitly told to widen it.
- No CI currently runs `go test` — `.github/workflows/warden.yml` only runs the Warden AI
  security-review skill on PRs. The operator is currently the sole collaborator with push access
  (verified via `gh api repos/latent-affect/gifsmith/collaborators`), which is what actually
  de-risks this today — there's no branch-protection rule enforcing it, so re-verify this if
  collaborators are ever added.
