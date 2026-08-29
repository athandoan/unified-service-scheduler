# AGENT.md — Working rules

How to work in this repo. Architecture, stack, APIs, SQL, and numbers live in `docs/SYSTEM_DESIGN.md`. Do not copy them here.

**Truth.** Brief: `docs/KeyloopCodingChallange.pdf`. Design: `docs/SYSTEM_DESIGN.md`. Working rules: this file. Architecture → design. Working rules → here. Conflict → ask; do not rewrite either file to “fix” it.

**Do not reverse user decisions.** Occupancy, ownership, scope, and rejected alternatives are locked in the design. Ask before changing them.

**Do not implement as a side effect of docs.** Docs work must not grow code, SQL, CI, or stubs.

**Tests must be able to break the occupancy invariant.** Happy-path alone is not enough. What to test is in the design and README.

**Honesty stays in the design.** Named residuals are accepted; do not hide them.

**Evaluate as they will.** Four dimensions: design clarity, logic, foresight; execution quality, correctness, tests; AI direction, verification, and ownership (**primary**); docs professionalism. Video is human, not an agent deliverable.

**Own AI output.** Do not ship unverified model text or code. Direct → verify → debug → own.

**Ambiguity.** Make a reasonable assumption and document it in `docs/SYSTEM_DESIGN.md`. Ask if it would reverse a lock.

**AI notes** have one append home per phase in `docs/AI_LOG.md`. All four phases — **System design**, **Implement**, **Test**, **Deploy** — use three short lines: **AI** / **Reviewed** / **Changed** — log only. Not this file, not process, not nits. The log is not truth: do not reverse locks from it, and do not rewrite the README Narrative from it.

Ask before reversing any of this.
