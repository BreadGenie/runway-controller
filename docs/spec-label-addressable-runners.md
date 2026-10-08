# Spec: label-addressable runners

Make adopting Runway a one-line change in a repository's workflow: replace `runs-on: ubuntu-latest`
with a Runway label. No local action, no work-folder flag, no per-job inputs.

This spec covers the part that must be true before that is possible: a runner image must know which
repository and which workspace layout it was baked for, so that a mismatched pairing is refused
before a job pays for it rather than failing partway through.

## Problem Statement

A team adopting Runway today cannot simply change one line. Their workflow has to reference a local
setup action, pass a work folder, and thread a runner choice through every job. That work is invisible
in a README and expensive in practice, and it is the difference between Runway being a managed runner
and being a bespoke integration.

The reason a repository needs that glue is that the baked image's fast path is tied to one repository
and one workspace layout by convention rather than by record. The bake script builds the bench at a
path it derives from the workspace; the image records only the path in its manifest; nothing in the
image says *which repository it is for*. A pool therefore carries an image that is either exactly
right or silently wrong for the job about to land on it.

That pairing has already failed once. An image was published with its bench under the runner's own
relative `_work` directory while Runway's controller registers runners with the absolute
`/home/runner/work`. The archive restored cleanly, matched its digest, and the job then reported the
bake as usable and failed at site creation with a message about `bench` that named nothing useful. It
took a full rebuild cycle and a review to find, because the failure appeared at job time and the image
looked healthy until a job ran.

Three consequences follow for a team trying to adopt Runway:

- They cannot tell, before adopting, whether a runner image suits their repository.
- The check that would tell them runs after they have paid for a VM, and after their job has started
  failing in a way that is hard to attribute.
- The workflow glue that works around it cannot be removed, because removing it reintroduces the
  failure.

## Solution

An image states which repository it was baked for, and where that repository's bench lives. Both are
properties of the image record, not conventions inside a build script.

- A pool's image declares a workspace repository and a bench root. The bake reads them from the image
  record rather than deriving them, and refuses to bake a repository the image is not declared for.
- An image with no declared repository carries no bake. It boots, runs stock GitHub Actions steps, and
  a repository with no matched bake builds its own bench the ordinary way. That is the path a new
  adopter takes.
- The pairing is checked when a runner is selected, so a mismatch is a refusal with a reason, not a
  job that fails at site creation.
- A scale set's name is the label a repository writes, so naming it after the machine is how a
  repository selects capacity by changing one line.

A team that adopts Runway changes one line in their workflow and nothing else.

## User Stories

1. As a team adopting Runway, I want to change only the `runs-on` line in an existing workflow, so
   that adopting a managed runner costs me an afternoon rather than a migration.
2. As a team adopting Runway, I want stock GitHub Actions steps to work unchanged, so that my build
   and test commands do not have to learn about Runway.
3. As a team adopting Runway, I want to know before I adopt whether a runner image suits my
   repository, so that I do not discover the answer from a failed job.
4. As a team adopting Runway, I want a clear refusal naming the cause when a pool's image does not
   suit my repository, so that I can fix the pairing without reading a bench error.
5. As a team adopting Runway, I want a refusal at runner selection rather than mid-job, so that I am
   not billed for a VM that was never going to work.
6. As a repository owner, I want `runs-on` to state the machine shape I want, so that changing my
   CI capacity is a pull request like any other.
7. As a repository owner, I want to pick from the shapes that actually exist, so that I am not
   inventing a specification per job and guessing which one is built.
8. As a repository owner, I want my existing self-hosted label to keep working during a migration,
   so that a workflow edit does not silently stop matching any runner.
9. As a repository owner, I want an unmatched label to fail loudly, so that a typo does not queue
   forever.
10. As an operator, I want a pool's image to declare what it is for, so that I can tell from the pool
    record whether it is safe to point at a given repository.
11. As an operator, I want adding capacity to be creating a pool, not editing a table, so that a
    published label can never disagree with what can be built.
12. As an operator, I want the shape a job actually got recorded, so that I can explain a slow job and
    attribute its cost.
13. As an operator, I want a bake to refuse a repository it was not declared for, so that a
    misconfigured build cannot publish an image that is wrong for everyone.
14. As an operator, I want an explicitly untargeted image to be buildable, so that a base image with
    no bake is a first-class thing rather than a gap.
15. As an operator, I want the image record to remain the authority on layout, so that the bake script
    and the job-time check cannot disagree about where the bench lives.
16. As a reviewer, I want the mismatch guard covered by a test that fails when the guard is removed, so
    that a guard cannot look tested while never being reached.
17. As a reviewer, I want the repository in a test's fixture to be stated rather than assumed, so
    that a test cannot pass by agreeing with itself.
18. As a maintainer, I want a repository with no matching bake to take the ordinary GitHub path
    unchanged, so that the fast path is an optimisation rather than a dependency.
19. As a maintainer, I want the layout check to cost nothing when an image carries no bake, so that
    untargeted images are not penalised for being general.
20. As a maintainer, I want the label resolution to be a single lookup, so that the rule is readable
    in one place and testable without a live pool.
21. As a maintainer, I want the pool's existing shape fields to keep working, so that a pool created
    before this change still boots something usable.
22. As a maintainer, I want the shape recorded on the lease rather than derived later, so that
    reconciliation does not depend on a label that may have changed.
23. As a team migrating away, I want a way to build an image for my repository specifically, so that I
    can opt into the fast path deliberately instead of by accident.
24. As a team migrating away, I want to be told what my label resolved to, so that I can confirm I got
    the capacity I intended.

## Implementation Decisions

- **The image record owns workspace layout.** A Virtual Machine Image gains a workspace repository and
  a bench root. Both are recorded when the image is published and are absent for an image that
  carries no bake. A bench root without a workspace repository is meaningless and is refused.
- **The bake reads the image record, not a default.** The bake script derives its build directory from
  the image's declared repository and layout. It no longer defaults to a path, because a default is
  what produced the published-but-unusable image.
- **A bake for an undeclared repository is refused.** The bake refuses unless it is building for the
  repository its image record declares, or is explicitly invoked to target one. This turns a silent
  mismatch into a build failure the operator sees immediately.
- **An image with no declared repository is a valid base.** It carries no bake, no manifest, and no
  bench. It is what a repository with no matching bake boots.
- **The pairing is checked at selection.** When a runner is selected for a job, the pool's image
  workspace repository is compared against the repository on the incoming job. A mismatch refuses the
  selection and records the reason. This happens before a VM is created.
- **The check is unconditional for targeted images and free for untargeted ones.** An image with no
  workspace repository imposes nothing, so the check short-circuits rather than comparing two empty
  values.
- **The job-time layout guard stays.** The existing bench-root comparison in the setup action remains
  as a cheap second line. It is redundant with the selection check and is kept because a guest can
  outlive the record that selected it.
- **The scale set's name is the label, and the name carries the shape.** A label written in `runs-on`
  is of the form `runway-<vcpu>vcpu-<memory>gb`, such as `runway-4vcpu-8gb`. GitHub routes a job to a
  scale set by matching one of that scale set's labels, and a scale set's name is one of them, so
  naming the scale set after the machine is what lets `runs-on` reach that machine and nothing else.
  The disk is absent because no label states one: it comes from the pool.
- **The grammar is read, never configured.** Adding a shape is creating a pool and naming its scale
  set after the machine. There is no label-to-shape table, so there is no way for a published label
  to disagree with what can be built.
- **A shape label is a guard, not a scheduler.** A scale set's runners are homogeneous, so a pool is
  one machine class. A label either names exactly the machine that pool builds, or names no shape at
  all and takes the pool's own. It cannot quietly ask for a different one, because a job written
  against the wrong label would run on a machine it was never admitted to, which is the same silent
  mismatch as a bench restored into the wrong workspace.
- **A pool refuses a name that contradicts it.** A scale set named `runway-4vcpu-8gb` against a pool
  building 1 vCPU and 2 GiB is refused when the pool saves, where it is one line to fix, rather than
  at acquisition. The name must also be routable: `runway`-prefixed, lowercase. The runway CLI
  enforces the same pattern, so an operator cannot create a name the pool will refuse.
- **A name that states no shape still works.** `runway-dogfood` states nothing to check and keeps its
  meaning, so a pool that predates shapes does not have to be renamed.
- **A pool's shape fields remain the default.** When a job's label names no shape, the pool's own
  shape is used. A pool created before this change therefore keeps booting the same machine.
- **The resolved shape is recorded on the lease.** The specification a job was given is stored at
  lease creation, so reconciliation and cost attribution do not depend on a label that may have
  changed since.
- **The scale-set message's request labels are the input.** The label written in `runs-on` arrives with
  the job message and is the only new signal the controller needs. No protocol change is required.
- **A label that fails loudly, rather than falling back.** A label that claims a shape but names one
  this pool does not serve, or that does not parse as a shape at all, is refused naming the cause. A
  label is only exempt when it never claimed to be a shape: `self-hosted`, `runway-dogfood`, `linux`.
  The alternative, silently taking the pool's shape, would run a job on a machine its own label did
  not ask for and report success.
- **A typo in `runs-on` queues at GitHub, not here.** A repository writing `runway-4vcup-8gb` when no
  scale set carries that label matches no scale set, so GitHub queues the job and no refusal is ever
  issued. That is GitHub's routing, not a gap in this rule, and the remedy is the pool check above:
  the only way such a name reaches a pool is if an operator created it, and the pool then refuses it.
- **A job that reaches a pool whose shape it did not ask for is refused.** This is the case a pool can
  still see, because the label matched some other label on the scale set.
- **A pool serves a repository or it does not.** A pool whose image is targeted at one repository is
  not offered jobs from another. This is a selection rule, not an admission rule, so a job for an
  unmatched repository is not queued at a pool that would refuse it.
- **The setup action becomes optional, not required.** With layout owned by the image record, a
  repository with no matched bake takes GitHub's ordinary path. The action remains available for
  repositories that want the fast path.
- **Auth and secrets stay a manual chore.** Repository registration, permissions and secret
  distribution are unchanged and out of this spec's scope. The goal is that the *code* a repository
  carries is one line.

## Testing Decisions

- **Test behaviour, not wiring.** A test asserts that a mismatched repository is refused with a
  reason, not that a particular function was called. Tests that pass only while the environment
  happens to match the developer's machine are the failure mode this work exists to prevent.
- **Every guard has a removal test.** For each refusal added here, the test suite must demonstrate
  that removing the guard makes the corresponding test fail. Two guards in the preceding work passed
  locally while never reaching their assertion, so this is checked mechanically rather than by
  reading.
- **The label resolution is tested as a pure function.** Labels in, specification out, with no pool and
  no Atlas client. It is the one seam for this decision and it needs no infrastructure.
- **Fixture repositories are stated.** Tests state the repository they simulate rather than relying on
  a default, so a test cannot agree with itself about which repository it is running.
- **Selection is tested at the highest seam available.** The repository/image pairing is exercised
  through the selection path with a stub Atlas client, not by calling the comparison directly, so the
  test covers the order the checks actually run in.
- **Prior art.** The bake and sealer already have refusal tests that assert on the refusal's message;
  the image manifest validator is tested against real archives built with each required directory
  removed. The pairing tests follow both patterns.
- **Environment is scrubbed.** The harness clears the runner's own environment variables before
  running a builder, and a test asserts that each variable it sets does not change what the builder
  does. A GitHub runner exports a tool-cache variable that changed which paths a test inspected, and
  that failure was invisible outside CI.

## Out of Scope

- Repository registration, permissions, and GitHub App installation.
- Secret distribution to guests.
- Usage metering, billing, and quota enforcement beyond what leases already record.
- Caching between jobs. The fast path is a prepared image, not a shared writable cache.
- Warm artifacts, memory snapshots, and boot-latency reduction.
- Fork pull-request isolation policy. It affects who may reach a pool, not how a job is labelled.
- Removing the setup action from repositories that use it today.
- Multiple bake targets inside one image.

## Further Notes

The order of work matters and is not the order of difficulty. The layout ownership work must land
first, because label resolution is additive and safe to defer, whereas the layout convention is a
correctness bug that has already shipped once and will ship again the next time a repository adopts
with a different name.

The pool-holds-several-shapes question is settled, by what GitHub will actually route: one pool
equals one shape, and a repository's label selects the pool. GitHub matches a job against a scale
set's labels, and a scale set's name is one of them, so a single scale set cannot present several
sizes and honour which one a job asked for. Naming the scale set after the machine makes the label
and the pool the same object, which is why there is no table and no clamping.

How a repository discovers which labels exist is still open. The pool list is the answer today, and
it is honest, because a label that no pool carries will not be built. It is not self-serve, though:
a repository owner has to ask which shapes exist. An API that reports the shapes a team actually has
runners for is the thing that makes this discoverable rather than published.

The withdrawn measurement in the performance report is the reason the fast path is worth having, and
also the reason it is worth being careful about: the three green runs that first looked convincing
were green because they were configured the same way as the bug.