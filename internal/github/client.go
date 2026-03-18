package github

import (
	"context"
	"fmt"
	"net/http"
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
