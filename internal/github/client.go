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

// IsLatestComment reports whether commentID is the most recent comment on the
// issue or PR (review comments and issue comments are checked separately).
// Used to suppress redundant quote prefixes when replying to the latest comment.
func (c *Client) IsLatestComment(ctx context.Context, repo string, number int, commentID int64, isReviewComment bool) (bool, error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return false, err
	}
	parts := strings.SplitN(repo, "/", 2)

	if isReviewComment {
		comments, _, err := client.PullRequests.ListComments(ctx, parts[0], parts[1], number, &gh.PullRequestListCommentsOptions{
			Sort:        "created",
			Direction:   "desc",
			ListOptions: gh.ListOptions{PerPage: 1},
		})
		if err != nil {
			return false, fmt.Errorf("list review comments: %w", err)
		}
		if len(comments) == 0 {
			return false, nil
		}
		return comments[0].GetID() == commentID, nil
	}

	// The per-issue comments endpoint ignores sort/direction and always returns
	// oldest first. Walk to the last page to find the most recent comment.
	probe, resp, err := client.Issues.ListComments(ctx, parts[0], parts[1], number, &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return false, fmt.Errorf("list comments: %w", err)
	}
	if resp == nil || resp.LastPage == 0 {
		// Single page or empty issue.
		if len(probe) == 0 {
			return false, nil
		}
		return probe[0].GetID() == commentID, nil
	}
	last, _, err := client.Issues.ListComments(ctx, parts[0], parts[1], number, &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 1, Page: resp.LastPage},
	})
	if err != nil {
		return false, fmt.Errorf("list comments page %d: %w", resp.LastPage, err)
	}
	if len(last) == 0 {
		return false, nil
	}
	return last[0].GetID() == commentID, nil
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
	reBuildNumber  = regexp.MustCompile(`\*\*Build Number:\*\*\s*(\d+)`)
	reInstallLink  = regexp.MustCompile(`\[Install build \d+\]\(([^)]+)\)`)
	reSectionTitle = regexp.MustCompile(`^([^\n(]+?)(?:\s*\(([^)]+)\))?\s*$`)
)

// FormatBuildStatus returns Telegram HTML summarizing the PR Preview build status.
// It finds the <!-- pr-preview-comment --> comment on the PR and extracts the
// build number and one entry per `### Platform (Channel)` section.
func (c *Client) FormatBuildStatus(ctx context.Context, repo string, number int) (string, error) {
	client, err := c.clientForRepo(ctx, repo)
	if err != nil {
		return "", err
	}

	parts := strings.SplitN(repo, "/", 2)

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

	buildMatch := reBuildNumber.FindStringSubmatch(previewBody)
	if buildMatch == nil {
		return "", nil
	}

	resultParts := []string{fmt.Sprintf("Build %s", buildMatch[1])}

	// Each `### Platform (Channel)` section contributes one entry. A section
	// with an install link becomes a clickable "Install on Platform"; without
	// one, fall back to "Platform via Channel" (e.g. iOS via TestFlight).
	sections := strings.Split(previewBody, "\n### ")
	for _, section := range sections[1:] {
		title, body, ok := strings.Cut(section, "\n")
		if !ok {
			continue
		}
		titleMatch := reSectionTitle.FindStringSubmatch(title)
		if titleMatch == nil {
			continue
		}
		platform := strings.TrimSpace(titleMatch[1])
		channel := strings.TrimSpace(titleMatch[2])

		if link := reInstallLink.FindStringSubmatch(body); link != nil {
			resultParts = append(resultParts, fmt.Sprintf(`<a href="%s">Install on %s</a>`, link[1], platform))
		} else if channel != "" {
			resultParts = append(resultParts, fmt.Sprintf("%s via %s", platform, channel))
		} else {
			resultParts = append(resultParts, platform)
		}
	}

	return strings.Join(resultParts, " ⋅ "), nil
}
