package github

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v69/github"
)

// Client wraps the GitHub API client authenticated as a GitHub App.
type Client struct {
	apps *gh.Client
	key  []byte
	appID int64
}

func NewClient(appID int64, privateKey []byte) (*Client, error) {
	transport, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create app transport: %w", err)
	}

	return &Client{
		apps:  gh.NewClient(&http.Client{Transport: transport}),
		key:   privateKey,
		appID: appID,
	}, nil
}

// clientForRepo returns a GitHub client authenticated as the app installation for the given repo.
func (c *Client) clientForRepo(ctx context.Context, repo string) (*gh.Client, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format: %s", repo)
	}

	installation, _, err := c.apps.Apps.FindRepositoryInstallation(ctx, parts[0], parts[1])
	if err != nil {
		return nil, fmt.Errorf("find installation for %s: %w", repo, err)
	}

	transport, err := ghinstallation.New(http.DefaultTransport, c.appID, installation.GetID(), c.key)
	if err != nil {
		return nil, fmt.Errorf("create installation transport: %w", err)
	}

	return gh.NewClient(&http.Client{Transport: transport}), nil
}

// GetQuoteContext fetches the text to quote in a reply.
// If commentID > 0, fetches that specific comment; otherwise fetches the issue/PR body.
func (c *Client) GetQuoteContext(ctx context.Context, repo string, number int, commentID int64, isReviewComment bool) (author, body string, err error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return "", "", err
	}

	parts := strings.SplitN(repo, "/", 2)

	if commentID > 0 {
		if isReviewComment {
			comment, _, err := client.PullRequests.GetComment(ctx, parts[0], parts[1], commentID)
			if err != nil {
				return "", "", fmt.Errorf("get review comment: %w", err)
			}
			return comment.GetUser().GetLogin(), comment.GetBody(), nil
		}
		comment, _, err := client.Issues.GetComment(ctx, parts[0], parts[1], commentID)
		if err != nil {
			return "", "", fmt.Errorf("get comment: %w", err)
		}
		return comment.GetUser().GetLogin(), comment.GetBody(), nil
	}

	issue, _, err := client.Issues.Get(ctx, parts[0], parts[1], number)
	if err != nil {
		return "", "", fmt.Errorf("get issue: %w", err)
	}
	return issue.GetUser().GetLogin(), issue.GetBody(), nil
}

// CreateComment posts a comment on an issue or PR and returns the new comment's ID.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (int64, error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return 0, err
	}

	parts := strings.SplitN(repo, "/", 2)
	comment, _, err := client.Issues.CreateComment(ctx, parts[0], parts[1], number, &gh.IssueComment{
		Body: gh.Ptr(body),
	})
	if err != nil {
		return 0, err
	}
	return comment.GetID(), nil
}

// CreateReviewReply posts a reply to an inline review comment and returns the new comment's ID.
func (c *Client) CreateReviewReply(ctx context.Context, repo string, number int, commentID int64, body string) (int64, error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return 0, err
	}

	parts := strings.SplitN(repo, "/", 2)
	reply, _, err := client.PullRequests.CreateCommentInReplyTo(ctx, parts[0], parts[1], number, body, commentID)
	if err != nil {
		return 0, err
	}
	return reply.GetID(), nil
}

// GetIssueOrPR fetches info about a GitHub issue or pull request.
func (c *Client) GetIssueOrPR(ctx context.Context, repo string, number int) (title, htmlURL, headSHA string, isPR bool, err error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return "", "", "", false, err
	}

	parts := strings.SplitN(repo, "/", 2)

	// Try as PR first
	pr, _, prErr := client.PullRequests.Get(ctx, parts[0], parts[1], number)
	if prErr == nil {
		return pr.GetTitle(), pr.GetHTMLURL(), pr.GetHead().GetSHA(), true, nil
	}

	// Fall back to issue
	issue, _, issueErr := client.Issues.Get(ctx, parts[0], parts[1], number)
	if issueErr != nil {
		return "", "", "", false, fmt.Errorf("get #%d: %w", number, issueErr)
	}
	return issue.GetTitle(), issue.GetHTMLURL(), "", false, nil
}

// GetCommitInfo fetches info about a commit.
func (c *Client) GetCommitInfo(ctx context.Context, repo string, sha string) (shortSHA, message, htmlURL string, err error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return "", "", "", err
	}

	parts := strings.SplitN(repo, "/", 2)
	commit, _, err := client.Repositories.GetCommit(ctx, parts[0], parts[1], sha, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("get commit %s: %w", sha, err)
	}

	msg := commit.GetCommit().GetMessage()
	if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
		msg = msg[:idx]
	}

	short := sha
	if len(short) > 7 {
		short = short[:7]
	}

	return short, msg, commit.GetHTMLURL(), nil
}

var (
	reBuildNumber = regexp.MustCompile(`\*\*Build Number:\*\*\s*(\d+)`)
	reInstallLink = regexp.MustCompile(`\[Install build \d+\]\(([^)]+)\)`)
	reDeployCheck = regexp.MustCompile(`- \[([ xX])\] <!-- deploy-(\w+) --> (.+)`)
)

// FormatBuildStatus returns Telegram HTML summarizing the PR Preview build status.
// It finds the <!-- pr-preview-comment --> comment on the PR and extracts
// the build number and install links.
func (c *Client) FormatBuildStatus(ctx context.Context, repo string, number int) (string, error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return "", err
	}

	parts := strings.SplitN(repo, "/", 2)

	// Find the PR preview comment
	comments, _, err := client.Issues.ListComments(ctx, parts[0], parts[1], number, &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return "", fmt.Errorf("list comments: %w", err)
	}

	var previewBody string
	for _, comment := range comments {
		if strings.HasPrefix(comment.GetBody(), "<!-- pr-preview-comment -->") {
			previewBody = comment.GetBody()
			break
		}
	}
	if previewBody == "" {
		return "", nil
	}

	// Extract build number
	buildMatch := reBuildNumber.FindStringSubmatch(previewBody)
	if buildMatch == nil {
		return "", nil
	}
	buildNumber := buildMatch[1]

	// Extract deploy checkboxes to determine platform status
	deployChecks := reDeployCheck.FindAllStringSubmatch(previewBody, -1)

	// Extract install links per platform section
	type platformInfo struct {
		checked bool
		label   string
		url     string
	}
	platforms := make(map[string]*platformInfo)

	for _, dc := range deployChecks {
		checked := dc[1] != " "
		name := dc[2]
		platforms[name] = &platformInfo{checked: checked, label: dc[3]}
	}

	// Parse install links from platform sections
	sections := strings.Split(previewBody, "### ")
	for _, section := range sections {
		link := reInstallLink.FindStringSubmatch(section)
		if link == nil {
			continue
		}
		sectionLower := strings.ToLower(section)
		for name, info := range platforms {
			if strings.HasPrefix(sectionLower, name) {
				info.url = link[1]
			}
		}
	}

	// Format output: "Build 680 ⋅ Install on Android ⋅ iOS skipped"
	var resultParts []string
	resultParts = append(resultParts, fmt.Sprintf("Build %s", buildNumber))

	type platformDef struct {
		key         string
		displayName string
	}
	platformDefs := []platformDef{
		{"android", "Android"},
		{"ios", "iOS"},
	}

	for _, pd := range platformDefs {
		info, ok := platforms[pd.key]
		if !ok {
			continue
		}
		if info.url != "" {
			resultParts = append(resultParts, fmt.Sprintf(`<a href="%s">Install on %s</a>`, info.url, pd.displayName))
		} else if info.checked {
			resultParts = append(resultParts, info.label)
		} else {
			resultParts = append(resultParts, pd.displayName+" skipped")
		}
	}

	return strings.Join(resultParts, " ⋅ "), nil
}
