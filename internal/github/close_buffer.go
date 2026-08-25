package github

import (
	"sync"
	"time"

	gh "github.com/google/go-github/v69/github"
)

const closeSettleTimeout = 500 * time.Millisecond

type closeKey struct {
	repo   string
	number int
}

type pendingClose struct {
	issue       *gh.IssuesEvent
	pullRequest *gh.PullRequestEvent
	comment     *gh.IssueCommentEvent
	timer       *time.Timer
}

type closeBuffer struct {
	mu      sync.Mutex
	pending map[closeKey]*pendingClose
	flush   func(closeKey)
}

func newCloseBuffer(flush func(closeKey)) *closeBuffer {
	return &closeBuffer{
		pending: make(map[closeKey]*pendingClose),
		flush:   flush,
	}
}

func closeEventKey(repo string, number int) closeKey {
	return closeKey{repo: repo, number: number}
}

func (b *closeBuffer) addIssue(e *gh.IssuesEvent) {
	key := closeEventKey(e.GetRepo().GetFullName(), e.GetIssue().GetNumber())
	b.add(key, e, nil)
}

func (b *closeBuffer) addPullRequest(e *gh.PullRequestEvent) {
	key := closeEventKey(e.GetRepo().GetFullName(), e.GetPullRequest().GetNumber())
	b.add(key, nil, e)
}

func (b *closeBuffer) add(key closeKey, issue *gh.IssuesEvent, pullRequest *gh.PullRequestEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.pending[key]
	if !ok {
		p = &pendingClose{}
		b.pending[key] = p
		p.timer = time.AfterFunc(reviewBufferTimeout, func() {
			b.flush(key)
		})
	}
	p.issue = issue
	p.pullRequest = pullRequest
}

// addComment attaches a created conversation comment only when the matching
// issue or pull request close is already pending.
func (b *closeBuffer) addComment(e *gh.IssueCommentEvent) bool {
	key := closeEventKey(e.GetRepo().GetFullName(), e.GetIssue().GetNumber())

	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.pending[key]
	if !ok || (p.issue == nil && p.pullRequest == nil) {
		return false
	}
	p.comment = e
	p.timer.Reset(closeSettleTimeout)
	return true
}

// take atomically removes and returns a pending close. A timer callback that
// loses this race becomes a no-op in the flush path.
func (b *closeBuffer) take(key closeKey) *pendingClose {
	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.pending[key]
	if !ok {
		return nil
	}
	delete(b.pending, key)
	return p
}
