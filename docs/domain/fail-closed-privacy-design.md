# Domain briefing — Fail-closed privacy guarantees & verify-before-release patterns
(compiled 2026-08-16, for task: GIFsmith fail-closed metadata scrub redesign)

**Confidence & ceiling:** Medium-High. The security-engineering vocabulary (fail-open/fail-closed/fail-secure)
and the atomic-write/verify-before-publish patterns are well-established, cross-checked across
independent sources (NIST, vendor security blogs, Go ecosystem docs). The redaction-failure case
law (Manafort, NSA guidance) is well-documented via primary/near-primary sources. The weakest
part of the ceiling: I found no canonical, named "verify-then-publish" pattern specific to
*binary metadata scrubbing* the way there is for builds (reproducible builds) or deployments
(atomic symlink flip) — that synthesis (applying the general pattern to a GIF-scrub step) is
mine, not lifted from a citable source. Treat section 4 of the Pitfalls below as the most
load-bearing/least-externally-verified claim in this brief. Recheck-frontier: none of this is
time-sensitive; the concepts are decades old (TOCTOU: 1990s Unix security literature; fail-safe:
predates computing).

## Vocabulary — terms of art

- **Fail-open** — a control that, on failure, defaults to the permissive/operational state
  (e.g., a firewall that passes all traffic if it crashes). Prioritizes availability over the
  guarantee. [AuthZed, "Understanding Fail Open and Fail Closed"](https://authzed.com/blog/fail-open)
- **Fail-closed / fail-secure** — a control that, on failure, defaults to the denial/blocked
  state. Prioritizes the guarantee over availability. NIST SP 800-53 codifies this as control
  **SC-7(18) "Fail Secure"** for boundary protection devices. [CSF Tools, SC-7(18)](https://csf.tools/reference/nist-sp-800-53/r5/sc/sc-7/sc-7-18/)
- **Fail-safe** — physical-security literature (locks, doors) sometimes uses "fail-safe" as a
  synonym for fail-open (unlocks on power loss, for human egress) and NIST's own glossary uses
  "fail safe" for a *different* concept (graceful termination that avoids damage to resources) —
  the terms are genuinely used inconsistently across sub-fields, so state the actual behavior,
  not just the label. [NIST CSRC Glossary, "fail safe"](https://csrc.nist.gov/glossary/term/fail_safe);
  [Axis, "Fail-safe vs fail-secure"](https://newsroom.axis.com/en-us/blog/fail-safe-vs-fail-secure)
- **TOCTOU (Time-of-Check-to-Time-of-Use)** — a race condition where a system verifies a
  resource's state and then acts on stale trust in that state; the resource can change in the
  gap. Classic Unix example: `access()`/`stat()` then `open()` on the same pathname, where a
  symlink swap in between redirects the later operation. [Wikipedia, "Time-of-check to
  time-of-use"](https://en.wikipedia.org/wiki/Time-of-check_to_time-of-use)
- **Atomic rename / write-temp-then-rename** — the standard technique for eliminating a whole
  class of TOCTOU and partial-write bugs: write new content to a temp file in the *same
  directory* as the target, `fsync`, then `rename()` it over the target. POSIX guarantees
  `rename()` is atomic (a reader sees either the fully-old or fully-new file, never a partial
  write); Windows requires `MoveFileEx` to get the same guarantee. [Michael Stapelberg,
  "Atomically writing files in Go"](https://michael.stapelberg.ch/posts/2017-01-28-golang_atomically_writing/);
  Go ecosystem reference implementations: [google/renameio](https://pkg.go.dev/github.com/google/renameio),
  [natefinch/atomic](https://github.com/natefinch/atomic)
- **Redaction vs. sanitization** — the NSA's own terminological distinction: *redaction* hides
  text from a reader at the presentation layer (a black box over rendered text); *sanitization*
  removes the underlying data from the file itself (metadata, hidden layers, an OCR text layer,
  embedded revision history). A tool that only does the former while claiming to do the latter is
  the exact bug class this brief is about. [NSA, "Redacting With Confidence" (mirrored by
  FAS)](https://fas.org/publication/nsa_redacting_with_confidence/)
- **Reproducible build / independent verification** — the build-security term for verifying a
  claim ("this binary matches this source") via a *second, independently derived* computation
  rather than trusting the artifact of the process being checked. Root concept: Ken Thompson's
  "trusting trust," countered by **Diverse Double-Compiling (DDC)** — building a compiler with
  itself proves nothing; building it with an *unrelated* second compiler and comparing output
  does. [reproducible-builds.org](https://reproducible-builds.org/); [Wheeler, "Fully Countering
  Trusting Trust through Diverse Double-Compiling"](https://www.researchgate.net/publication/245578769_Fully_Countering_Trusting_Trust_through_Diverse_Double-Compiling)

## Canon — the load-bearing sources

- **NIST SP 800-53 Rev.5, SC-7(18) "Fail Secure"** — the formal control definition: on failure of
  a boundary protection mechanism, the system must not enter an unsecure state. This is the
  citable authority for "fail-closed is the named, standard security control for this exact
  situation," not an informal blog preference. [csf.tools/SC-7(18)](https://csf.tools/reference/nist-sp-800-53/r5/sc/sc-7/sc-7-18/)
- **NSA, "Redacting With Confidence: How to Safely Publish Sanitized Reports Converted From Word
  to PDF" (2005)** — the primary government guidance that named this exact failure class before
  it became famous via court filings. Core finding: converting formats or drawing a visual
  overlay does not remove underlying data; the data must be structurally deleted, and the
  guidance walks through *verifying* the output rather than trusting the tool that did the edit.
  Ironically, commenters later found the NSA's own PDF still carried leftover embedded metadata —
  cited below as a Pitfall. [FAS mirror](https://fas.org/publication/nsa_redacting_with_confidence/);
  discussion: [Schneier on Security, "The NSA on How to
  Redact"](https://www.schneier.com/blog/archives/2006/02/the_nsa_on_how.html)
- **The Manafort redaction failure (Jan 2019)** — the canonical real-world instance of "looks
  redacted, isn't." Manafort's lawyers submitted a court PDF with black rectangles drawn over
  text; the underlying text layer was untouched, and journalists recovered it by copy-pasting
  into a text editor within hours. This is the concrete, citable case for "a scrub that succeeds
  visually/structurally-adjacent but not at the actual data layer is not success." [Vice, "How a
  Simple Copy/Paste Revealed Explosive New Detail in Manafort's
  Case"](https://www.vice.com/en/article/paul-manafort-russia-case-redaction-fail/); [CJR
  analysis](https://www.cjr.org/analysis/manafort-mueller-redacted-document-ukraine.php)
- **Atomic deployment / symlink-flip pattern (release engineering canon)** — build the new
  release into a sibling directory, run health checks against *that* artifact, and only then
  atomically flip a symlink (or `rename()`) to make it live; failure at any stage before the flip
  leaves the previously-live artifact untouched and serving. This is the general-purpose ancestor
  of what GIFsmith's fix needs to do at file-write granularity instead of deployment granularity.
  [DeployHQ, "The Easiest Way to Set Up Zero Downtime Deployments (Atomic
  Symlinks)"](https://www.deployhq.com/blog/what-s-the-easiest-way-to-set-up-zero-downtime-deployments)
- **Sigstore/cosign image-digest verification** — a modern, widely-deployed example of
  "verify-then-admit" gating: an admission controller checks a cryptographic signature over an
  immutable content digest *before* allowing an artifact to run, structurally separate from the
  process that built the artifact. Useful as the pattern-shape (independent verifier, gate before
  use) even though the crypto machinery is overkill for a local single-process tool.
  [docs.sigstore.dev, cosign verifying](https://docs.sigstore.dev/cosign/verifying/verify/)

## Pitfalls — what practitioners know that outsiders get wrong

- **"Fail-closed" is not free — it's a stated tradeoff against availability, not a universal
  upgrade.** Multiple sources are explicit that neither mode is unconditionally more secure: fail
  open risks the guarantee being silently bypassed; fail closed risks denial-of-service turning
  into its own operational or even safety problem (e.g., a fail-secure door in a fire is the wrong
  choice). The honest framing is "which failure mode matches what's actually at stake here," not
  "closed is more secure, always." [AuthZed](https://authzed.com/blog/fail-open); [Axis, fail-safe
  vs fail-secure](https://newsroom.axis.com/en-us/blog/fail-safe-vs-fail-secure) — for GIFsmith
  specifically this tradeoff resolves cleanly: the "availability" being sacrificed is just "the
  job fails and the user reruns it," while the guarantee being protected is deanonymization risk
  on copyrighted source material — an easy call, but it's worth stating *why* it's easy rather
  than treating fail-closed as self-justifying.
- **Verifying with the same code path that might have the bug proves nothing.** This is the
  entire lesson of "trusting trust" / Diverse Double-Compiling: a check built from the same logic
  being checked will reliably pass when that logic has a shared blind spot, and will not catch a
  bug in the write/serialize step by re-running the same write/serialize step. The practically
  relevant form for GIFsmith: verifying the scrub by checking the in-memory `cleaned []byte` that
  `stripGIFComments` just produced is not verification — it's restating the same computation's
  output. Real verification has to inspect the bytes that actually landed on disk, ideally via a
  logically distinct check (e.g., a raw byte-pattern scan for the known gifski Comment Extension
  signature, independent of the structural parser that removed it) — not just "did `err == nil`."
- **A file that "opens fine" is not proof it's not silently corrupted or incompletely written.**
  Partial/interrupted writes are the classic case: a process crash, a full disk, or an interrupted
  syscall mid-`os.WriteFile` can leave a truncated file that still has a valid-looking GIF header
  and still decodes in a permissive viewer, while being neither the original nor the fully-cleaned
  version. Plain `os.WriteFile` to the final path is a direct write-in-place, not atomic, and
  offers no such guarantee. [Stapelberg, "Atomically writing files in
  Go"](https://michael.stapelberg.ch/posts/2017-01-28-golang_atomically_writing/)
- **Redaction/sanitization tools have a track record of failing at exactly the "looks done"
  layer, not the "obviously broken" layer.** Both the Manafort case and the irony noted in the
  Schneier/NSA thread (the NSA's own redaction guidance PDF reportedly still carried leftover
  metadata) point at the same lesson: the dangerous failure mode isn't a crash, it's a file that
  displays correctly, passes a casual glance, and still carries the thing you meant to remove.
  This directly motivates *not* trusting "no error + file exists + file opens" as sufficient
  success criteria — those are exactly the properties the Manafort PDF and NSA's own PDF had.
- **TOCTOU applies even inside a single-process, single-job tool, not just multi-tenant/networked
  systems.** The classic TOCTOU literature is about attacker-controlled races (symlink swaps
  between `stat()` and `open()`), which understates its relevance here — but the *structural* gap
  (verify state X, then later act as if X still holds) is exactly what happens if GIFsmith
  verifies a temp file's cleanliness and then a separate step later reads/serves/moves that file
  under a different path or after other code has touched it. The fix pattern (verify the artifact
  you are about to atomically publish, in the same step that publishes it, with no gap for
  anything else to touch the file) closes this regardless of whether an "attacker" is involved —
  it also incidentally closes the interrupted-write case, since a half-written temp file simply
  never gets renamed into place. [Wikipedia, TOCTOU](https://en.wikipedia.org/wiki/Time-of-check_to_time-of-use)

## Frontier — current state

Nothing in this domain is fast-moving; the core ideas (fail-secure controls, atomic rename,
TOCTOU, trusting-trust/independent verification) are all decades-old and stable as of 2026-08-16.
The one actively-evolving area is *supply-chain* verify-before-publish tooling (sigstore/cosign,
policy-controller admission gating), which is more sophisticated than GIFsmith needs but confirms
the pattern-shape ("verify an immutable artifact before anything downstream can consume it") is
still the field's current best practice, not a legacy idea being superseded by something looser.
[docs.sigstore.dev](https://docs.sigstore.dev/cosign/verifying/verify/) (2026 docs, actively maintained)

## Open gaps — what search couldn't establish

- No single named pattern was found for "verify-then-publish applied to a metadata-scrub step on
  a single binary file" — the closest citable canon is at the *deployment* (symlink flip) and
  *build* (reproducible builds) granularity, not file-mutation granularity. The application below
  is a reasonable synthesis of those two, not a lifted pattern with its own name. Treat as
  medium-confidence engineering judgment, not canon.
- Did not find a public postmortem/incident writeup specific to a *GIF or image metadata
  stripping* tool failing silently (exiftool searches returned only usage docs, no documented
  incident) — the closest and best-documented real-world analog remains the PDF/document
  redaction case law (Manafort, NSA), which is a different file format but the identical failure
  *shape* (visible/structural cleanup succeeded, underlying data didn't). If a closer analog
  matters later, a targeted search of CVE databases for "steghide," "exiftool," or "mat2"
  (Metadata Anonymisation Toolkit — a purpose-built privacy tool with a more directly comparable
  threat model) would be the next place to look; mat2 in particular positions itself exactly in
  this space and was not investigated here.

---

## Concrete synthesis for GIFsmith (grounded in current code)

Read for this brief: `/Users/m5/dev/gif-smith/gifsmith/gifmeta.go` (the parser,
`stripGIFComments`) and `/Users/m5/dev/gif-smith/gifsmith/pipeline.go` lines 536-563
(`scrubOutputMetadata`, the current fail-open caller).

Current shape: `scrubOutputMetadata` is called with no error return; on any failure (read error,
parse error, write error) it calls `Debug.Add(...)` — writing to the hidden ring buffer described
in the task — and silently returns, leaving `outPath` exactly as the encoder wrote it (fingerprint
intact) while `pipeline.go`'s caller reports the job as successful. The write-back at line 560
(`os.WriteFile(outPath, cleaned, 0o600)`) is a direct in-place write, not atomic — an interrupted
write here corrupts the delivered file in a third, worse way (neither original nor cleaned).

The three canon patterns above compose directly into the fix:

1. **Fail-secure by construction (NIST SC-7(18) shape):** `scrubOutputMetadata` must return an
   `error`, and any error from it must fail the whole job — no code path may report success while
   `outPath` still holds unscrubbed encoder output. This turns "logs and continues" into
   "logs and aborts," which is the entire fail-open→fail-closed change in one sentence.
2. **Write-temp, independently re-verify, then atomic-rename (deployment/build-canon shape):**
   write `cleaned` to a temp file in the same directory as `outPath` (not `outPath` itself); after
   writing, re-open *that temp file from disk* — not the in-memory `cleaned` slice — and run an
   independent check for the gifski Comment Extension signature (ideally a dumb byte-pattern scan
   for `0x21 0xFE` at a top-level block boundary, distinct from trusting `stripGIFComments`'s own
   `changed`/`err` return, per the trusting-trust pitfall above); only if that independent check
   passes, `rename()` the temp file over `outPath`. If parsing fails, if the write fails, or if
   the independent re-check still finds a comment block, delete the temp file and fail the job —
   never fall through to leaving the original (fingerprinted) `outPath` in place, and never
   publish a half-written temp file.
3. **No serving/reading gap (TOCTOU shape):** because the rename only happens after the
   independent re-verify, and nothing else in a single-process, one-job-at-a-time tool can touch
   `outPath` in between, this collapses the TOCTOU window to zero by construction rather than by
   discipline — there is no step where GIFsmith reports success and the caller could read the file
   before the verify has happened, because success is reported by the rename itself.

Net effect: a scrub failure now can only ever produce "job failed, no file delivered," never
"job succeeded, file delivered, fingerprint intact, user unaware" — which is exactly the
structural guarantee the operator described wanting.
