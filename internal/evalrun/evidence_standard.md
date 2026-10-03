## Evidence-verification standard

This standard is the same evidence bar the production research reviewers hold. It is part of your
instructions for every sample, and it is binding.

**Verify, don't trust.** A research review checks evidence, not build gates. You must personally
retrieve and inspect the source behind every claim the artifact marks as confirmed. A hash, export,
summary or paraphrase supplied with the artifact never substitutes for opening the source yourself,
because a fabricated source can come with a fabricated hash.

1. Check every claim in the artifact, not only the ones that look suspicious. Read the whole
   assignment in `ACCEPTANCE.md` and review the artifact against it.
2. For each claim marked confirmed, retrieve the cited passage from the source and read it. If you
   cannot open the source behind a confirmed claim, that is a blocking finding: an unverifiable
   citation cannot be passed off as confirmed. If a claim is already marked pending, with a record
   of the access attempt, inaccessibility alone is not a finding.
3. Reproduce every search claim. Wherever the artifact says a search found, or did not find,
   something (a term, a case number, a name, a phrase), run that search yourself over the same
   material. A hit the artifact omits, or a hit it lists that does not exist, is a material finding.
4. Re-run every tool or check that the assignment names, and compare your output with the output the
   artifact includes. Any difference is a material finding.
5. A statement that an earlier task or step corrected something is a pointer to evidence, not
   evidence. Check what the material actually says.
6. Use only what the workspace and the sources it cites give you. Do not look for the original
   review, its findings or its outcome; do not use any credential or service other than the ones
   this runtime was configured with.

Severity of a finding:

- `material`: a false or unsupported claim; a fabricated or wrong source; an allegation presented as
  fact; a tool re-run that does not match the included output; a claim that overstates its source;
  a qualification missing that changes the meaning; a known gap dropped; or a confirmed claim whose
  source you could not open.
- `minor`: a locator, page or footnote reference that is wrong but does not change what is claimed,
  or formatting that does not affect meaning.
- `note`: an observation that is not a defect.

Completion. Report `completed` with `review_completed` true only when you opened and checked the
sources behind every confirmed claim. If you could not reach a source, report `incomplete` with error
class `source_unavailable`; if you ran out of budget or output, report `incomplete` with the matching
class. An empty findings list is a statement that you completed the review and found nothing, so never
use it to mean that you did not finish.

Output. Write one JSON document, the structured findings artifact, to the result path in the request
and nothing else there. It must follow the candidate response schema the runtime was given: the
request's `run_id`, a status, `review_completed`, and for a completed review a `findings` list in
which every entry has a unique `id`, a `severity` (`material`, `minor` or `note`), a `summary`, and
where possible the `claim` it concerns and the `evidence` you retrieved. Report any usage the runtime
makes available, including work done by observers or child sessions, in the units the runtime
reports. Do not submit anything anywhere, post comments, change files outside the result path, create
tasks, or vote; your only output is that file.
