package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KCaverly/caretaker/internal/repo"
)

// prRecord is a PR reduced to what the reconciler needs. It differs from the
// public PR type by carrying the head ref and raw state — fields used to match
// PRs to commits and to classify them — that the JSON output does not expose.
type prRecord struct {
	Number    int
	URL       string
	Title     string
	Body      string // PR body, needed to splice the nav table on submit
	State     string // OPEN, CLOSED, MERGED
	Draft     bool
	Head      string // headRefName
	HeadSHA   string // headRefOid; survives branch deletion on GitHub
	Base      string // baseRefName
	Review    string // reviewDecision
	Mergeable string // MERGEABLE, CONFLICTING, UNKNOWN
	MergedAt  string // "" when never merged
	Checks    Checks
}

// ghPR mirrors one PullRequest node of the batched GraphQL query. The check
// rollup arrives nested under the head commit (that is where GitHub hangs it),
// and its contexts are a heterogeneous array of CheckRun and StatusContext
// objects, so ghCheck captures the fields of both shapes and summarizeChecks
// reconciles them.
type ghPR struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	HeadRefName    string `json:"headRefName"`
	HeadRefOid     string `json:"headRefOid"`
	BaseRefName    string `json:"baseRefName"`
	ReviewDecision string `json:"reviewDecision"`
	Mergeable      string `json:"mergeable"`
	MergedAt       string `json:"mergedAt"`
	Commits        struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup struct {
					Contexts struct {
						Nodes []ghCheck `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// checks flattens the rollup hanging off the PR's head commit. Every nullable
// level in that path (no commits selected, no rollup attached) decodes to a zero
// value, so an unchecked PR simply yields no contexts.
func (p ghPR) checks() []ghCheck {
	if len(p.Commits.Nodes) == 0 {
		return nil
	}
	return p.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes
}

// ghCheck is the union of the two node shapes GitHub returns in a check rollup:
// a CheckRun (Name + Status + Conclusion) or a legacy StatusContext (Context +
// State).
type ghCheck struct {
	Name       string `json:"name"`       // CheckRun
	Status     string `json:"status"`     // CheckRun: QUEUED, IN_PROGRESS, COMPLETED
	Conclusion string `json:"conclusion"` // CheckRun: SUCCESS, FAILURE, …
	Context    string `json:"context"`    // StatusContext
	State      string `json:"state"`      // StatusContext: SUCCESS, PENDING, FAILURE, ERROR
}

const (
	// ghTimeout bounds the gh subprocess for the same reason repo.Git bounds git:
	// a credential helper or a stalled API call must fail visibly rather than hang.
	ghTimeout = 30 * time.Second

	// ghRefsPerQuery caps how many head branches one GraphQL document asks about.
	// The query is scoped to this worktree's namespace, so a stack blows past this
	// only after accumulating a lot of never-cleaned-up branches; chunking keeps a
	// single request's server-side cost bounded either way.
	ghRefsPerQuery = 25

	// ghPRsPerRef is how many PRs to read per head branch, newest first. A
	// well-formed stack has exactly one; the surplus covers a branch that
	// accumulated a stale closed PR beside a newer one, which resolveCommit's
	// precedence is written to handle. It is deliberately small: this is the
	// factor GitHub multiplies the per-PR check page by when it reserves a node
	// budget, so 3 rather than 5 asks for 40% less (measured: 2,615 -> 1,569
	// nodes for a five-branch stack) while still covering the only shape that
	// puts more than one PR on a stack branch.
	ghPRsPerRef = 3

	// ghChecksPerPR matches the page size `gh pr list --json statusCheckRollup`
	// uses, so the check summary is computed from the same window gh would report.
	ghChecksPerPR = 100

	// ghRetries is how many times a transient (5xx / timeout) GitHub failure is
	// retried before it is reported. GitHub sheds load under contention rather
	// than failing outright, so a single retry recovers most of them.
	ghRetries = 2

	// ghRetryBackoff is the first retry delay; it doubles per attempt.
	ghRetryBackoff = 400 * time.Millisecond
)

// ghRef is one head branch in a gather's scope, plus how much of its PRs the
// reconciler can actually use. Tracked marks a branch a local commit's ct-stack-id
// trailer names: those PRs drive every per-commit state, so they are read in full
// and in every state. An untracked branch is a namespace branch left in the fetched
// refs with no local commit behind it — the only thing it can contribute is an
// orphan report, so it is probed for open PRs only.
type ghRef struct {
	Branch  string
	Tracked bool
}

// gatherGitHub reads this worktree's stack PRs and returns them as prRecords
// alongside a GitHub availability report.
//
// The query is scoped to the head branches this stack could possibly own —
// derived locally, from the commits' ct-stack-id trailers and the fetched
// refs/remotes/origin/ct/<worktree>/ refs — rather than listing the repository's
// PRs and filtering afterwards. That is what keeps the cost proportional to the
// stack instead of to the repository: asking GitHub for a 200-PR window of check
// rollups makes it fan out to every check run on every one of those PRs, which on
// a busy repository exceeds the GraphQL time limit and 504s (issue #56). It also
// removes a correctness cliff — on a repository with more PRs than the old
// window, this stack's own older PRs could fall outside it and read as missing.
//
// Any problem — gh not installed, a non-zero exit (which includes the
// unauthenticated case), or undecodable output — is reported as available=false
// with a warning and an empty slice, never a hard error: the caller must still be
// able to render the local stack shape offline.
func gatherGitHub(dir, worktree, mainBranch string) ([]prRecord, GitHub) {
	refs, err := stackHeadRefs(dir, worktree, mainBranch)
	if err != nil {
		return nil, GitHub{Available: false, Warnings: []string{"could not read local stack branches: " + err.Error()}}
	}
	return gatherGitHubFor(dir, worktree, refs)
}

// gatherGitHubFor is gatherGitHub with the head-branch scope supplied by the
// caller, for the pipelines that already computed it and must not pay for the
// git reads twice.
func gatherGitHubFor(dir, worktree string, refs []ghRef) ([]prRecord, GitHub) {
	gh := GitHub{Available: false, Warnings: []string{}}

	if _, err := exec.LookPath("gh"); err != nil {
		gh.Warnings = append(gh.Warnings, "gh CLI not found on PATH; GitHub PR status unavailable")
		return nil, gh
	}

	// No branch this stack could own means there is nothing to ask GitHub about,
	// and the GitHub half of the status is vacuously complete. A fresh worktree
	// therefore does no network work at all.
	if len(refs) == 0 {
		gh.Available = true
		return nil, gh
	}

	slug, err := resolveGHRepo(dir)
	if err != nil {
		gh.Warnings = append(gh.Warnings, "could not resolve the GitHub repository: "+err.Error())
		return nil, gh
	}

	prs, err := queryStackPRs(dir, slug, refs)
	if err != nil {
		gh.Warnings = append(gh.Warnings, describeGHFailure(err))
		return nil, gh
	}

	gh.Available = true
	return filterStackPRs(prs, worktree), gh
}

// stackHeadRefs lists every ct/<worktree>/ branch that could carry one of this
// stack's PRs, without touching the network: one per local commit's ct-stack-id
// trailer (which names the branch even after GitHub deleted it on merge), plus
// every branch under the namespace in the last-fetched remote refs (which is what
// surfaces an orphan PR whose commit is gone locally).
//
// The two sources are exactly the two ways a stack PR can be reachable, so the
// scope is complete for everything the reconciler asks of it. The one case it
// cannot see is an open PR whose head branch was deleted on GitHub *and* whose
// commit no longer exists locally; that PR is unreachable from either side and is
// left to the human, as an orphan of an orphan.
func stackHeadRefs(dir, worktree, mainBranch string) ([]ghRef, error) {
	commits, err := localCommits(dir, mainBranch)
	if err != nil {
		return nil, err
	}
	remotes, err := remoteBranches(dir, worktree)
	if err != nil {
		return nil, err
	}
	return headRefsFrom(worktree, commits, remotes), nil
}

// headRefsFrom is stackHeadRefs over data the caller already read, so a gather
// that ran the git log and the ref walk does not run them a second time.
func headRefsFrom(worktree string, commits []LocalCommit, remotes map[string]string) []ghRef {
	prefix := "ct/" + worktree + "/"
	seen := map[string]bool{}
	var refs []ghRef
	add := func(id string, tracked bool) {
		branch := prefix + id
		if seen[branch] {
			return
		}
		seen[branch] = true
		refs = append(refs, ghRef{Branch: branch, Tracked: tracked})
	}

	// Tracked first, so a branch that is both a live commit's and a lingering
	// remote ref's is read in full rather than probed.
	for _, c := range commits {
		if validStackID(c.StackID) {
			add(c.StackID, true)
		}
	}

	ids := make([]string, 0, len(remotes))
	for id := range remotes {
		ids = append(ids, id)
	}
	// Map order is random; sorting keeps the query document (and so the response
	// order) stable across runs.
	sort.Strings(ids)
	for _, id := range ids {
		add(id, false)
	}

	return refs
}

// queryStackPRs runs the batched head-ref query, in chunks of ghRefsPerQuery, and
// returns every PR found across them newest-first. Sorting by number descending
// restores the ordering `gh pr list` gave (creation order), which matchPRs relies
// on to prefer the newest PR of each state on a branch — the response's own
// ordering is per-alias and the aliases come back as a map.
func queryStackPRs(dir string, slug ghRepo, refs []ghRef) ([]ghPR, error) {
	var all []ghPR
	for start := 0; start < len(refs); start += ghRefsPerQuery {
		chunk := refs[start:min(start+ghRefsPerQuery, len(refs))]
		out, err := runGHRetry(dir, ghQueryArgs(slug, chunk)...)
		if err != nil {
			return nil, err
		}
		prs, err := decodeGHPRs([]byte(out))
		if err != nil {
			return nil, err
		}
		all = append(all, prs...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Number > all[j].Number })
	return all, nil
}

// ghQueryArgs builds the argv for one batched query. Kept pure and separate from
// the runner so the exact request is unit-testable without invoking gh.
func ghQueryArgs(slug ghRepo, refs []ghRef) []string {
	args := []string{"api", "graphql"}
	if slug.Host != "" && slug.Host != "github.com" {
		args = append(args, "--hostname", slug.Host)
	}
	return append(args,
		"-f", "owner="+slug.Owner,
		"-f", "name="+slug.Name,
		"-f", "query="+ghQueryDocument(refs))
}

// ghQueryDocument builds a GraphQL document that asks for one head branch per
// aliased field, so every branch in the chunk is answered in a single round trip.
//
// Selecting by headRefName reads the repository's pull-request index directly —
// unlike a `--search` query, it is not served from GitHub's search index, so a PR
// created or retargeted moments ago is visible immediately. The submit and merge
// pipelines verify their own mutations with this query, and search-index lag would
// make those verifications flap.
//
// Each branch gets one of two selections. A tracked branch — one a local commit's
// trailer names — is read in full and across every state, because a MERGED PR on
// it is what tells the engine a commit has landed. An untracked branch is only
// ever consulted to surface an orphan, which is by definition an *open* PR, so it
// is probed with states:[OPEN] and the four fields an orphan report needs. That
// asymmetry is what keeps a worktree's accumulated history of landed branches from
// costing anything: their probes match nothing and never touch a check rollup or
// a PR body.
func ghQueryDocument(refs []ghRef) string {
	var b strings.Builder
	anyTracked, anyProbe := false, false
	b.WriteString("query($owner:String!,$name:String!){repository(owner:$owner,name:$name){")
	for i, ref := range refs {
		if ref.Tracked {
			anyTracked = true
			fmt.Fprintf(&b, "r%d:pullRequests(headRefName:%s,first:%d,orderBy:{field:CREATED_AT,direction:DESC}){nodes{...pr}}",
				i, strconv.Quote(ref.Branch), ghPRsPerRef)
			continue
		}
		anyProbe = true
		fmt.Fprintf(&b, "r%d:pullRequests(headRefName:%s,states:[OPEN],first:%d,orderBy:{field:CREATED_AT,direction:DESC}){nodes{...openPr}}",
			i, strconv.Quote(ref.Branch), ghPRsPerRef)
	}
	b.WriteString("}}")
	// Only the fragments in use may appear: GraphQL rejects a document that
	// defines one and never spreads it, so an all-tracked stack must not carry
	// the probe fragment (or vice versa).
	if anyTracked {
		fmt.Fprintf(&b, `fragment pr on PullRequest{`+
			`number url title body state isDraft headRefName headRefOid baseRefName reviewDecision mergeable mergedAt `+
			`commits(last:1){nodes{commit{statusCheckRollup{contexts(first:%d){nodes{`+
			`__typename ... on CheckRun{name status conclusion} ... on StatusContext{context state}`+
			`}}}}}}}`, ghChecksPerPR)
	}
	if anyProbe {
		b.WriteString(`fragment openPr on PullRequest{number url state headRefName baseRefName}`)
	}
	return b.String()
}

// ghResponse is the envelope `gh api graphql` prints: every aliased head-ref
// field lands as a key of the repository object, which decodes as a map because
// the alias names are generated.
type ghResponse struct {
	Data struct {
		Repository map[string]struct {
			Nodes []ghPR `json:"nodes"`
		} `json:"repository"`
	} `json:"data"`
}

// decodeGHPRs unmarshals the batched query's response into a flat PR list. A
// separate function so the JSON contract with gh is unit-testable against a
// fixture.
func decodeGHPRs(data []byte) ([]ghPR, error) {
	var resp ghResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var prs []ghPR
	for _, field := range resp.Data.Repository {
		prs = append(prs, field.Nodes...)
	}
	return prs, nil
}

// filterStackPRs keeps only PRs whose head branch is under this worktree's
// ct/<worktree>/ namespace and collapses each into a prRecord, summarizing its
// check rollup. The query is already scoped to those branches; the filter stays
// as the guarantee the reconciler depends on, so a widened scope can never leak a
// foreign PR into the stack.
func filterStackPRs(prs []ghPR, worktree string) []prRecord {
	prefix := "ct/" + worktree + "/"
	var records []prRecord
	for _, p := range prs {
		if !strings.HasPrefix(p.HeadRefName, prefix) {
			continue
		}
		records = append(records, prRecord{
			Number:    p.Number,
			URL:       p.URL,
			Title:     p.Title,
			Body:      p.Body,
			State:     p.State,
			Draft:     p.IsDraft,
			Head:      p.HeadRefName,
			HeadSHA:   p.HeadRefOid,
			Base:      p.BaseRefName,
			Review:    p.ReviewDecision,
			Mergeable: p.Mergeable,
			MergedAt:  p.MergedAt,
			Checks:    summarizeChecks(p.checks()),
		})
	}
	return records
}

// summarizeChecks collapses a PR's heterogeneous check rollup into a single
// summary word plus the names of failing checks. Precedence is failing > pending
// > passing: one red check makes the whole rollup "failing" (so the next-action
// engine says fix-ci before it says wait), and an empty rollup is "none".
func summarizeChecks(checks []ghCheck) Checks {
	c := Checks{Summary: "none", Failing: []string{}}
	if len(checks) == 0 {
		return c
	}

	anyPending, anyFailing := false, false
	for _, ck := range checks {
		switch classifyCheck(ck) {
		case "failing":
			anyFailing = true
			c.Failing = append(c.Failing, checkName(ck))
		case "pending":
			anyPending = true
		}
	}

	switch {
	case anyFailing:
		c.Summary = "failing"
	case anyPending:
		c.Summary = "pending"
	default:
		c.Summary = "passing"
	}
	return c
}

// classifyCheck maps one check node to "passing", "failing", or "pending",
// handling both the CheckRun shape (status/conclusion) and the StatusContext
// shape (state). Unknown values are treated as pending — the conservative choice
// that keeps the engine from prompting a merge on a check it doesn't understand.
func classifyCheck(ck ghCheck) string {
	// CheckRun: not COMPLETED means still running/queued.
	if ck.Status != "" && ck.Status != "COMPLETED" {
		return "pending"
	}
	if ck.Conclusion != "" {
		switch ck.Conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			return "passing"
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			return "failing"
		default:
			return "pending"
		}
	}
	// StatusContext shape.
	switch ck.State {
	case "SUCCESS":
		return "passing"
	case "FAILURE", "ERROR":
		return "failing"
	case "PENDING", "EXPECTED", "":
		return "pending"
	default:
		return "pending"
	}
}

// checkName returns the display name of a check, preferring the CheckRun Name and
// falling back to the StatusContext Context (or a placeholder when both empty).
func checkName(ck ghCheck) string {
	switch {
	case ck.Name != "":
		return ck.Name
	case ck.Context != "":
		return ck.Context
	default:
		return "(unnamed check)"
	}
}

// requireGH is submit's hard precondition: gh must be on PATH. Status soft-fails
// when gh is missing, but submit cannot open or edit PRs without it, so it fails
// early with a clear message. (Authentication is verified implicitly by the
// status gather, which reports github.available=false when gh is unauthed.)
func requireGH() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh CLI not found on PATH; stack submit needs GitHub access")
	}
	return nil
}

// ghCreateArgs builds the argv for creating a stack PR: it heads the given
// branch, bases on the previous commit's branch (or main for the bottom), and
// carries the title and full body. --draft is appended when requested. Kept pure
// and separate from the runner so the exact argv is unit-testable without ever
// invoking gh.
func ghCreateArgs(head, base, title, body string, draft bool) []string {
	args := []string{"pr", "create", "--head", head, "--base", base, "--title", title, "--body", body}
	if draft {
		args = append(args, "--draft")
	}
	return args
}

// ghEditBaseArgs builds the argv to retarget a PR onto a new base branch.
func ghEditBaseArgs(number int, base string) []string {
	return []string{"pr", "edit", strconv.Itoa(number), "--base", base}
}

// ghEditTitleArgs builds the argv to update a PR's title after a commit subject
// changed.
func ghEditTitleArgs(number int, title string) []string {
	return []string{"pr", "edit", strconv.Itoa(number), "--title", title}
}

// ghEditBodyArgs builds the argv to replace a PR's body (used for the nav-table
// splice).
func ghEditBodyArgs(number int, body string) []string {
	return []string{"pr", "edit", strconv.Itoa(number), "--body", body}
}

// ghCreatePR creates a PR via the gh CLI. Mutating: never called under
// --dry-run.
func ghCreatePR(dir, head, base, title, body string, draft bool) error {
	_, err := runGH(dir, ghCreateArgs(head, base, title, body, draft)...)
	return err
}

// ghEditBase retargets a PR's base branch. Mutating.
func ghEditBase(dir string, number int, base string) error {
	_, err := runGH(dir, ghEditBaseArgs(number, base)...)
	return err
}

// ghEditTitle updates a PR's title. Mutating.
func ghEditTitle(dir string, number int, title string) error {
	_, err := runGH(dir, ghEditTitleArgs(number, title)...)
	return err
}

// ghEditBody replaces a PR's body. Mutating.
func ghEditBody(dir string, number int, body string) error {
	_, err := runGH(dir, ghEditBodyArgs(number, body)...)
	return err
}

// ghPRBase reads one PR's state and base branch — the whole query, no rollup and
// no body. It is what the mutating pipelines verify a retarget with: a full stack
// gather would answer the same question at many times the cost, and this one
// scales with nothing at all.
func ghPRBase(dir string, number int) (state, base string, err error) {
	out, err := runGHRetry(dir, "pr", "view", strconv.Itoa(number), "--json", "state,baseRefName")
	if err != nil {
		return "", "", err
	}
	var v struct {
		State       string `json:"state"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", "", err
	}
	return v.State, v.BaseRefName, nil
}

// ensureBranchesHaveNoOpenDependents is the final guard before deleting remote
// stack branches. The check is intentionally fresh rather than derived from an
// earlier StackStatus: another actor may have retargeted or opened a PR while a
// multi-step submit/restack operation was running. It takes the whole batch so a
// multi-branch cleanup pays for one gather rather than one per branch.
func ensureBranchesHaveNoOpenDependents(dir, worktree, mainBranch string, branches []string) error {
	if len(branches) == 0 {
		return nil
	}
	prs, gh := gatherGitHub(dir, worktree, mainBranch)
	if !gh.Available {
		return fmt.Errorf("refusing to delete %s while GitHub state is unavailable: %s",
			strings.Join(branches, ", "), strings.Join(gh.Warnings, "; "))
	}
	for _, branch := range branches {
		if dependents := openPRsBasedOn(prs, branch); len(dependents) > 0 {
			return fmt.Errorf("refusing to delete %s: open PR #%d still targets it", branch, dependents[0].Number)
		}
	}
	return nil
}

// ensureBranchHasNoOpenDependents guards a single branch delete.
func ensureBranchHasNoOpenDependents(dir, worktree, mainBranch, branch string) error {
	return ensureBranchesHaveNoOpenDependents(dir, worktree, mainBranch, []string{branch})
}

func openPRsBasedOn(prs []prRecord, branch string) []prRecord {
	var out []prRecord
	for _, p := range prs {
		if p.State == "OPEN" && p.Base == branch {
			out = append(out, p)
		}
	}
	return out
}

// ghRepo is the owner/name (and, for GitHub Enterprise, host) triple the GraphQL
// query needs. `gh pr list` infers it from the working directory; `gh api
// graphql` cannot, so it is resolved explicitly.
type ghRepo struct{ Owner, Name, Host string }

// ghRepoCache memoises the resolution per worktree directory. A worktree's remote
// does not change under a running process, and the fallback path costs a network
// round trip, so paying it once per directory keeps the polling callers (watch,
// the post-merge settle loop, the TUI's passive refresh) free of it.
var ghRepoCache sync.Map // dir -> ghRepo

func resolveGHRepo(dir string) (ghRepo, error) {
	if v, ok := ghRepoCache.Load(dir); ok {
		return v.(ghRepo), nil
	}
	r, err := resolveGHRepoUncached(dir)
	if err != nil {
		return ghRepo{}, err
	}
	ghRepoCache.Store(dir, r)
	return r, nil
}

// resolveGHRepoUncached mirrors gh's own repository resolution, cheapest first.
// `gh repo set-default` records its answer in git config, and a remote URL parses
// locally, so the common cases cost no network at all; asking gh itself is the
// last resort for a remote shape this cannot read.
func resolveGHRepoUncached(dir string) (ghRepo, error) {
	// `gh repo set-default` writes remote.<name>.gh-resolved: either an explicit
	// owner/repo, or "base" meaning that remote's own URL is the answer.
	if out, err := repo.Git(dir, "config", "--get-regexp", `^remote\..*\.gh-resolved$`); err == nil {
		for _, line := range strings.Split(out, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			if value != "base" {
				if owner, name, ok := strings.Cut(value, "/"); ok && owner != "" && name != "" {
					return ghRepo{Owner: owner, Name: name}, nil
				}
				continue
			}
			remote := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".gh-resolved")
			if r, ok := remoteGHRepo(dir, remote); ok {
				return r, nil
			}
		}
	}

	// gh's preference order when nothing is pinned: a fork's PRs live upstream.
	for _, remote := range []string{"upstream", "github", "origin"} {
		if r, ok := remoteGHRepo(dir, remote); ok {
			return r, nil
		}
	}

	// Nothing local parsed — let gh answer, at the cost of a round trip.
	out, err := runGH(dir, "repo", "view", "--json", "owner,name")
	if err != nil {
		return ghRepo{}, err
	}
	var v struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return ghRepo{}, err
	}
	if v.Owner.Login == "" || v.Name == "" {
		return ghRepo{}, fmt.Errorf("gh repo view returned no owner/name")
	}
	return ghRepo{Owner: v.Owner.Login, Name: v.Name}, nil
}

func remoteGHRepo(dir, remote string) (ghRepo, bool) {
	out, err := repo.Git(dir, "remote", "get-url", remote)
	if err != nil {
		return ghRepo{}, false
	}
	return parseRemoteURL(strings.TrimSpace(out))
}

// parseRemoteURL pulls host/owner/name out of the remote URL forms git uses:
// scp-style (git@host:owner/repo.git) and any scheme:// URL. Anything it cannot
// read is reported as not-a-match so the caller falls through to asking gh.
func parseRemoteURL(raw string) (ghRepo, bool) {
	if raw == "" {
		return ghRepo{}, false
	}
	host, path := "", ""
	if scheme, rest, ok := strings.Cut(raw, "://"); ok && scheme != "" {
		hostPart, p, ok := strings.Cut(rest, "/")
		if !ok {
			return ghRepo{}, false
		}
		// Strip any user@ and :port.
		if _, after, ok := strings.Cut(hostPart, "@"); ok {
			hostPart = after
		}
		host, _, _ = strings.Cut(hostPart, ":")
		path = p
	} else {
		hostPart, p, ok := strings.Cut(raw, ":")
		if !ok {
			return ghRepo{}, false
		}
		if _, after, ok := strings.Cut(hostPart, "@"); ok {
			hostPart = after
		}
		host, path = hostPart, p
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || host == "" || owner == "" || name == "" || strings.Contains(name, "/") {
		return ghRepo{}, false
	}
	return ghRepo{Owner: owner, Name: name, Host: host}, true
}

// describeGHFailure turns a gh error into a diagnosis the reader can act on.
// The old wording guessed at authentication for every failure, which sent people
// to re-run `gh auth login` over what was really GitHub shedding load; the 5xx
// and auth cases are distinguishable from gh's stderr, so they are distinguished.
func describeGHFailure(err error) string {
	msg := err.Error()
	switch {
	case transientGHFailure(msg):
		return "GitHub API is temporarily unavailable (transient 5xx/timeout, retried " +
			strconv.Itoa(ghRetries) + "x); PR status omitted: " + msg
	case containsAny(msg, "HTTP 401", "HTTP 403", "Bad credentials", "gh auth login", "authentication"):
		return "gh is not authenticated for this repository: " + msg
	case containsAny(msg, "HTTP 404", "Could not resolve to a Repository", "no git remotes found"):
		return "GitHub repository not found or not accessible: " + msg
	default:
		return "gh query failed: " + msg
	}
}

// transientGHFailure reports whether a gh error is the kind a retry can fix: a
// 5xx, a timeout, or a dropped connection. Rate limiting is deliberately not in
// here — retrying it immediately makes it worse.
func transientGHFailure(msg string) bool {
	return containsAny(msg,
		"HTTP 500", "HTTP 502", "HTTP 503", "HTTP 504",
		"couldn't respond to your request in time",
		"context deadline exceeded", "timeout", "Timeout", "timed out",
		"connection reset", "EOF", "no such host", "TLS handshake",
	)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// runGHRetry runs gh, retrying a transient GitHub failure with exponential
// backoff. Read-only queries only: every caller is a gather or a verification, so
// a retry can never double-apply a mutation.
func runGHRetry(dir string, args ...string) (string, error) {
	var out string
	var err error
	for attempt := 0; ; attempt++ {
		out, err = runGH(dir, args...)
		if err == nil {
			return out, nil
		}
		if attempt >= ghRetries || !transientGHFailure(err.Error()) {
			return "", err
		}
		time.Sleep(ghRetryBackoff << attempt)
	}
}

// runGH runs a gh command in dir with the same 30s-timeout, stderr-wrapping
// contract as repo.Git. It is a local twin rather than a reuse of repo.Git
// because that runner is hard-wired to the git binary.
//
// The wrapped error names the subcommand rather than the whole argv: a GraphQL
// document is kilobytes long and burying the actual failure under it is what made
// the old 504 read as noise.
func runGH(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", ghCommandName(args), msg)
	}
	return stdout.String(), nil
}

// ghCommandName is the leading, non-flag part of a gh argv — "api graphql",
// "pr edit 12" — used to name a failure without echoing its payload.
func ghCommandName(args []string) string {
	var parts []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		parts = append(parts, a)
	}
	if len(parts) == 0 {
		return "gh"
	}
	return strings.Join(parts, " ")
}
