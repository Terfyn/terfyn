package native

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/Terfyn/terfyn/internal/jsonnum"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools/toolctx"
)

// GitHub list (read) operations. Each is a GET returning a JSON array, decoded to a
// slice under a named key. Optional filters are passed through as query parameters.
//
// githubWalkArray is the package's one pagination loop: it follows Link rel="next"
// (100 items/page) for at most githubListMaxPages pages. The list ops and the
// post_comment marker search (githubFindAgenticReviewCommentID) both use it, so they
// share end-of-list and cap semantics: the list ends only when a response carries no
// rel="next"; a next link that is present but cannot be safely followed (foreign
// origin, scheme downgrade, malformed) is an error when the caller still needs more;
// and reaching the cap with a next link remaining is never reported as a complete
// walk. The bearer token only goes to URLs produced by githubResolveSameOrigin
// (githubGETURL takes a githubVettedURL), including the first page.
//
// List results are bounded by what the consumer can receive, not only by page
// count: every tool output passes the tool-output limit its caller enforces (the
// resolved project/workflow/tool maxToolOutputBytes, 256 KiB and truncate policy by
// default), and 100 real issue or PR objects already exceed the default; a workflow
// step's output must also fit the run's checkpoint. The walk therefore stops once the
// next whole item would push the encoded items past the byte budget
// (githubListOutputBudget: three quarters of the budget the caller advertises), or
// once the caller's optional `limit` is reached, and reports truncated=true when
// anything was left unreturned. Items are never cut: the result is always a prefix
// of GitHub's order, made of whole objects.

const (
	githubListPerPage  = 100
	githubListMaxPages = 10
	// githubListMaxLimit is the largest `limit` a list op accepts: what the page cap
	// can ever return.
	githubListMaxLimit = githubListPerPage * githubListMaxPages
	// githubListDefaultMaxOutputBytes is the item byte budget when the call context
	// carries no resolved tool-output limit (a direct Registry.Call or Dispatch): three
	// quarters of the default limit. See githubListOutputBudget.
	githubListDefaultMaxOutputBytes = spec.DefaultMaxToolOutputBytes * 3 / 4
)

// githubListOutputBudget bounds the JSON-encoded size of the items one list call
// returns. It is three quarters of the output budget the caller advertises for this
// call (tools.OutputBudget, set from spec.ResolveExecutionLimits), so the whole result
// (items, the result key, and the truncated flag) passes that limit untouched with
// headroom for the envelope. The MCP server's PolicyDispatcher advertises the resolved
// maxToolOutputBytes. The engine's runToolStep advertises the smaller of that and half
// the run's maxCheckpointBytes, because a workflow step's output must also fit the
// checkpoint, where a suspension stores it twice (engine toolOutputBudget). So raising
// maxToolOutputBytes for a GitHub tool returns more items only up to that clamp; past
// it, raise maxCheckpointBytes as well. The clamp is per call: every list step in a
// workflow shares one checkpoint, and keeping their sum (and every other step's
// output) within maxCheckpointBytes is the operator's responsibility; a run that
// exceeds it fails at the checkpoint, not at the list.
// Without a budget on the context it falls back to githubListDefaultMaxOutputBytes. A
// non-positive budget means the caller enforces no output limit, so only `limit` and
// the page cap bound the walk. A single item larger than the budget is still returned
// alone (see githubGETArray); the caller's output limit then applies to it.
func githubListOutputBudget(ctx context.Context) int {
	n, ok := toolctx.OutputBudget(ctx)
	switch {
	case !ok:
		return githubListDefaultMaxOutputBytes
	case n <= 0:
		return math.MaxInt
	default:
		return n - n/4 // 3/4 of n without overflowing n*3
	}
}

// githubPullRequestList lists pull requests: GET /repos/{owner}/{repo}/pulls.
// Optional filters: state (open|closed|all), head, base; optional limit.
func githubPullRequestList(ctx context.Context, with map[string]any) (map[string]any, error) {
	return githubListOp(ctx, with, "pull_request.list", "pulls", "pull_requests", "state", "head", "base")
}

// githubIssuesList lists issues: GET /repos/{owner}/{repo}/issues.
// Optional filters: state (open|closed|all), labels (comma-separated); optional limit.
func githubIssuesList(ctx context.Context, with map[string]any) (map[string]any, error) {
	return githubListOp(ctx, with, "issues.list", "issues", "issues", "state", "labels")
}

// githubListOp runs one list op against /repos/{owner}/{repo}/{resource} and returns
// {key: [...items], "truncated": bool}. truncated is always present: false means
// the list is complete.
func githubListOp(ctx context.Context, with map[string]any, op, resource, key string, filters ...string) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, op)
	if err != nil {
		return nil, err
	}
	limit, err := githubListLimit(with, op)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/%s%s", url.PathEscape(owner), url.PathEscape(repo), resource, githubListQuery(with, limit, filters...))
	arr, truncated, err := githubGETArray(ctx, path, op, limit)
	if err != nil {
		return nil, err
	}
	if arr == nil {
		arr = []any{}
	}
	return map[string]any{key: arr, "truncated": truncated}, nil
}

// githubListLimit reads the optional `limit` (maximum number of items to return),
// defaulting to githubListMaxLimit. Out-of-range values are an error, not clamped.
func githubListLimit(with map[string]any, op string) (int, error) {
	n, ok, err := optionalIntFromWith(with, "limit")
	if err != nil {
		return 0, fmt.Errorf("native: %s %w", op, err)
	}
	if !ok {
		return githubListMaxLimit, nil
	}
	if n < 1 || n > githubListMaxLimit {
		return 0, fmt.Errorf("native: %s field \"limit\" must be between 1 and %d, got %d", op, githubListMaxLimit, n)
	}
	return n, nil
}

// githubListQuery builds a "?k=v&…" string from the given optional string fields
// plus per_page (githubListPerPage, or limit if smaller) so GitHub does not silently
// default to 30 and a small limit costs one small request.
func githubListQuery(with map[string]any, limit int, fields ...string) string {
	vals := url.Values{}
	for _, f := range fields {
		if v, ok := tryStringFromWith(with, f); ok {
			vals.Set(f, v)
		}
	}
	vals.Set("per_page", strconv.Itoa(min(limit, githubListPerPage)))
	return "?" + vals.Encode()
}

// githubGETArray walks a list endpoint and returns its items as a prefix of GitHub's
// order: whole items only, at most limit of them, and at most
// githubListOutputBudget(ctx) bytes of JSON-encoded items (the first item is always kept, so an
// oversized object cannot make the list look empty; the engine's generic output limit
// then applies to it as it does to issues.get).
//
// Contract: a nil error with truncated=false means the list is complete. truncated is
// true whenever an item was left unreturned (limit, byte budget, or the page cap with
// a next link remaining). A next page that must be fetched but cannot be followed
// safely is an error, never a partial list presented as complete.
func githubGETArray(ctx context.Context, path, op string, limit int) ([]any, bool, error) {
	var (
		items    []any
		size     int
		leftover bool
		encErr   error
		budget   = githubListOutputBudget(ctx)
	)
	more, err := githubWalkArray(ctx, path, op, func(page []any) bool {
		for i, it := range page {
			if len(items) >= limit {
				leftover = i < len(page)
				return false
			}
			b, err := json.Marshal(it)
			if err != nil {
				encErr = err
				return false
			}
			n := len(b)
			if len(items) > 0 {
				n++ // separating comma
				if size+n > budget {
					leftover = true
					return false
				}
			}
			items = append(items, it)
			size += n
		}
		return len(items) < limit
	})
	if err != nil {
		return nil, false, err
	}
	if encErr != nil {
		return nil, false, fmt.Errorf("native: %s measure item: %w", op, encErr)
	}
	return items, leftover || more, nil
}

// githubWalkArray fetches the JSON-array endpoint at the API-relative path page by
// page. visit gets each page's items in order and returns false once it needs no
// more pages. more reports that the walk ended with a next page advertised but not
// fetched: either visit stopped early, or githubListMaxPages was reached. When visit
// still wants more and the advertised next link cannot be followed safely, the walk
// fails rather than ending as if the list were complete.
func githubWalkArray(ctx context.Context, path, op string, visit func(page []any) bool) (more bool, err error) {
	if !strings.HasPrefix(path, "/") {
		return false, fmt.Errorf("native: %s: list path must be API-relative", op)
	}
	abs := githubAPIBase() + path
	current, reason := githubResolveSameOrigin(abs, abs)
	if reason != "" {
		return false, fmt.Errorf("native: %s: refusing list request: %s", op, reason)
	}
	for page := 1; ; page++ {
		b, hdr, err := githubGETURL(ctx, current, githubAcceptJSON, maxGitHubJSONBody)
		if err != nil {
			return false, err
		}
		var arr []any
		if err := jsonnum.Unmarshal(b, &arr); err != nil {
			return false, fmt.Errorf("native: %s decode: %w", op, err)
		}
		next, state, reason := githubFollowNext(current.String(), hdr.Values("Link"))
		wantMore := visit(arr)
		switch {
		case state == githubNextNone:
			return false, nil
		case !wantMore:
			return true, nil
		case state == githubNextRejected:
			return false, fmt.Errorf("native: %s: response advertises a next page (Link rel=\"next\") that cannot be followed safely (%s); refusing to return a partial list as complete", op, reason)
		case page == githubListMaxPages:
			return true, nil
		}
		current = next
	}
}

// githubVettedURL is an absolute URL that passed githubResolveSameOrigin. It is the
// only URL type githubGETURL accepts, so the bearer token reaches a response-supplied
// URL only after the same-origin check.
type githubVettedURL struct{ u *url.URL }

func (v githubVettedURL) String() string {
	if v.u == nil {
		return ""
	}
	return v.u.String()
}

// githubNextState is the tri-state result of inspecting a Link header for a next page.
type githubNextState int

const (
	// githubNextNone: no rel="next" entry; the list is complete.
	githubNextNone githubNextState = iota
	// githubNextFollow: a rel="next" entry that passed the same-origin check.
	githubNextFollow
	// githubNextRejected: a rel="next" entry exists but is malformed or unsafe to follow.
	githubNextRejected
)

// githubFollowNext inspects Link header values from the response to currentURL and
// decides whether the advertised next page may be fetched with the bearer token.
// For githubNextFollow it returns the vetted URL to request; for githubNextRejected
// it returns a short reason (containing no credentials).
func githubFollowNext(currentURL string, linkValues []string) (githubVettedURL, githubNextState, string) {
	target, present := parseGitHubLinkNext(strings.Join(linkValues, ", "))
	if !present {
		return githubVettedURL{}, githubNextNone, ""
	}
	if target == "" {
		return githubVettedURL{}, githubNextRejected, "rel=\"next\" entry has no usable target"
	}
	next, reason := githubResolveSameOrigin(currentURL, target)
	if reason != "" {
		return githubVettedURL{}, githubNextRejected, reason
	}
	return next, githubNextFollow, ""
}

// githubResolveSameOrigin resolves target against currentURL and returns it as a
// githubVettedURL only if it has exactly the configured GITHUB_API_URL origin (scheme, host, and
// port, with the scheme's default port normalized: https 443, http 80), carries no
// userinfo, and stays under the API base path. Otherwise it returns a non-empty reason.
func githubResolveSameOrigin(currentURL, target string) (githubVettedURL, string) {
	base, err := url.Parse(githubAPIBase())
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" {
		return githubVettedURL{}, "configured GITHUB_API_URL is not an absolute http(s) URL"
	}
	cur, err := url.Parse(currentURL)
	if err != nil {
		return githubVettedURL{}, "current request URL is unparseable"
	}
	ref, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return githubVettedURL{}, "target is not a valid URL"
	}
	u := cur.ResolveReference(ref)
	if u.User != nil {
		return githubVettedURL{}, "URL contains userinfo"
	}
	if u.Opaque != "" {
		return githubVettedURL{}, "URL is opaque"
	}
	if githubOrigin(u) != githubOrigin(base) {
		return githubVettedURL{}, fmt.Sprintf("URL origin %s does not match API origin %s", githubOrigin(u), githubOrigin(base))
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return githubVettedURL{}, "URL path contains dot segments"
		}
	}
	bp := strings.TrimSuffix(base.Path, "/")
	if bp != "" && u.Path != bp && !strings.HasPrefix(u.Path, bp+"/") {
		return githubVettedURL{}, "URL path is outside the API base path"
	}
	return githubVettedURL{u: u}, ""
}

// githubOrigin renders scheme://host:port in lower case with the scheme's default
// port made explicit, so https://h and https://h:443 compare equal but http://h,
// https://h, and https://h:8443 do not. Callers must have checked the scheme is
// http or https before trusting a match; a non-http(s) scheme yields an origin that
// can never equal a validated base.
func githubOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + ":" + port
}

// parseGitHubLinkNext scans a Link header for a rel="next" entry. present reports
// whether any entry names rel=next, even if its target is malformed (target is then
// empty), so callers can tell "no next page" from "a next page we cannot read".
// The scanner honors '>' as the target terminator, so commas inside <...> do not
// split entries.
func parseGitHubLinkNext(link string) (target string, present bool) {
	rest := link
	for {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			return target, present
		}
		var entry string
		if rest[0] == '<' {
			gt := strings.IndexByte(rest, '>')
			if gt < 0 {
				entry, rest = rest, ""
			} else {
				end := strings.IndexByte(rest[gt:], ',')
				if end < 0 {
					entry, rest = rest, ""
				} else {
					entry, rest = rest[:gt+end], rest[gt+end+1:]
				}
			}
		} else {
			// No target; consume through the next comma.
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				entry, rest = rest, ""
			} else {
				entry, rest = rest[:end], rest[end+1:]
			}
		}
		t, isNext := parseLinkEntry(entry)
		if isNext {
			if !present {
				target = t
			}
			present = true
		}
	}
}

// parseLinkEntry parses one Link entry. isNext is true when its rel names "next"
// (or, for an entry too malformed to parse, when it mentions "next" at all, so an
// unreadable continuation is never mistaken for the end of the list).
func parseLinkEntry(entry string) (target string, isNext bool) {
	entry = strings.TrimSpace(entry)
	lt := strings.IndexByte(entry, '<')
	gt := strings.IndexByte(entry, '>')
	if lt != 0 || gt <= lt {
		return "", strings.Contains(strings.ToLower(entry), "next")
	}
	target = strings.TrimSpace(entry[lt+1 : gt])
	if !linkRelIsNext(entry[gt+1:]) {
		return "", false
	}
	return target, true
}

// linkRelIsNext reports whether the parameters carry rel=next (rel may hold a
// space-separated list of relation types).
func linkRelIsNext(params string) bool {
	for _, p := range strings.Split(params, ";") {
		p = strings.TrimSpace(p)
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(k), "rel") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		for _, rel := range strings.Fields(v) {
			if strings.EqualFold(rel, "next") {
				return true
			}
		}
	}
	return false
}
