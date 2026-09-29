package native

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// GitHub list (read) operations. Each is a GET returning a JSON array, decoded to a
// slice under a named key. Optional filters are passed through as query parameters.
// List ops follow Link rel="next" up to githubListMaxPages (100 items/page), matching
// githubFindAgenticReviewCommentID. A still-present next link after the bound is
// reported as truncated rather than presented as a complete list. A next link that
// is present but cannot be safely followed (foreign origin, scheme downgrade,
// malformed) is an error, never a silently complete list, and the bearer token is
// only ever sent to URLs that pass githubFollowNext.

const (
	githubListPerPage  = 100
	githubListMaxPages = 10
)

// githubPullRequestList lists pull requests: GET /repos/{owner}/{repo}/pulls.
// Optional filters: state (open|closed|all), head, base.
func githubPullRequestList(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "pull_request.list")
	if err != nil {
		return nil, err
	}
	q := githubListQuery(with, "state", "head", "base")
	path := fmt.Sprintf("/repos/%s/%s/pulls%s", url.PathEscape(owner), url.PathEscape(repo), q)
	arr, truncated, err := githubGETArray(ctx, path, "pull_request.list")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"pull_requests": arr}
	if truncated {
		out["truncated"] = true
	}
	return out, nil
}

// githubIssuesList lists issues: GET /repos/{owner}/{repo}/issues.
// Optional filters: state (open|closed|all), labels (comma-separated).
func githubIssuesList(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "issues.list")
	if err != nil {
		return nil, err
	}
	q := githubListQuery(with, "state", "labels")
	path := fmt.Sprintf("/repos/%s/%s/issues%s", url.PathEscape(owner), url.PathEscape(repo), q)
	arr, truncated, err := githubGETArray(ctx, path, "issues.list")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"issues": arr}
	if truncated {
		out["truncated"] = true
	}
	return out, nil
}

// githubListQuery builds a "?k=v&…" string from the given optional string fields
// plus per_page=githubListPerPage so GitHub does not silently default to 30.
func githubListQuery(with map[string]any, fields ...string) string {
	vals := url.Values{}
	for _, f := range fields {
		if v, ok := tryStringFromWith(with, f); ok {
			vals.Set(f, v)
		}
	}
	vals.Set("per_page", strconv.Itoa(githubListPerPage))
	return "?" + vals.Encode()
}

// githubGETArray performs GitHub GETs expected to return JSON arrays, following
// Link rel="next" until the list ends or githubListMaxPages is reached.
//
// Contract: a nil error with truncated=false means the list is complete. If a
// response advertises a next page that we refuse to follow (or cannot parse), the
// call fails: returning the pages fetched so far would present a partial list as
// complete, and there is no field to carry the reason on the caller's result. The
// page-cap case keeps its existing truncated=true signal because the walk itself
// was bounded, not broken.
func githubGETArray(ctx context.Context, path, op string) ([]any, bool, error) {
	current := githubAbsoluteURL(path)
	var all []any
	for page := 1; page <= githubListMaxPages; page++ {
		b, hdr, err := githubRequest(ctx, "GET", current, githubAcceptJSON, maxGitHubJSONBody)
		if err != nil {
			return nil, false, err
		}
		var arr []any
		if err := json.Unmarshal(b, &arr); err != nil {
			return nil, false, fmt.Errorf("native: %s decode: %w", op, err)
		}
		all = append(all, arr...)
		next, state, reason := githubFollowNext(current, hdr.Values("Link"))
		switch state {
		case githubNextNone:
			return all, false, nil
		case githubNextRejected:
			return nil, false, fmt.Errorf("native: %s: response advertises a next page (Link rel=\"next\") that cannot be followed safely (%s); refusing to return a partial list as complete", op, reason)
		}
		if page == githubListMaxPages {
			return all, true, nil
		}
		current = next
	}
	return all, true, nil
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
// For githubNextFollow it returns the absolute URL to request; for githubNextRejected
// it returns a short reason (containing no credentials).
func githubFollowNext(currentURL string, linkValues []string) (string, githubNextState, string) {
	target, present := parseGitHubLinkNext(strings.Join(linkValues, ", "))
	if !present {
		return "", githubNextNone, ""
	}
	if target == "" {
		return "", githubNextRejected, "rel=\"next\" entry has no usable target"
	}
	next, reason := githubResolveSameOrigin(currentURL, target)
	if reason != "" {
		return "", githubNextRejected, reason
	}
	return next, githubNextFollow, ""
}

// githubResolveSameOrigin resolves target against currentURL and returns the absolute
// URL only if it has exactly the configured GITHUB_API_URL origin (scheme, host, and
// port, with the scheme's default port normalized: https 443, http 80), carries no
// userinfo, and stays under the API base path. Otherwise it returns a non-empty reason.
func githubResolveSameOrigin(currentURL, target string) (string, string) {
	base, err := url.Parse(githubAPIBase())
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" {
		return "", "configured GITHUB_API_URL is not an absolute http(s) URL"
	}
	cur, err := url.Parse(currentURL)
	if err != nil {
		return "", "current request URL is unparseable"
	}
	ref, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return "", "next link is not a valid URL"
	}
	u := cur.ResolveReference(ref)
	if u.User != nil {
		return "", "next link contains userinfo"
	}
	if u.Opaque != "" {
		return "", "next link is opaque"
	}
	if githubOrigin(u) != githubOrigin(base) {
		return "", fmt.Sprintf("next link origin %s does not match API origin %s", githubOrigin(u), githubOrigin(base))
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return "", "next link path contains dot segments"
		}
	}
	bp := strings.TrimSuffix(base.Path, "/")
	if bp != "" && u.Path != bp && !strings.HasPrefix(u.Path, bp+"/") {
		return "", "next link path is outside the API base path"
	}
	return u.String(), ""
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
