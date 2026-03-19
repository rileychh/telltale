package github

import (
	"sync"
	"time"

	gh "github.com/google/go-github/v69/github"
)

const reviewBufferTimeout = 30 * time.Second

type pendingReview struct {
	review   *gh.PullRequestReviewEvent
	comments []*gh.PullRequestReviewCommentEvent
	timer    *time.Timer
}

type reviewBuffer struct {
	mu      sync.Mutex
	pending map[int64]*pendingReview
	flush   func(int64)
}

func newReviewBuffer(flush func(int64)) *reviewBuffer {
	return &reviewBuffer{
		pending: make(map[int64]*pendingReview),
		flush:   flush,
	}
}

// addReview buffers a review event. May trigger a flush.
func (rb *reviewBuffer) addReview(e *gh.PullRequestReviewEvent) {
	reviewID := e.GetReview().GetID()

	rb.mu.Lock()
	p, ok := rb.pending[reviewID]
	if !ok {
		p = &pendingReview{}
		rb.pending[reviewID] = p
		p.timer = time.AfterFunc(reviewBufferTimeout, func() {
			rb.flush(reviewID)
		})
	}
	p.review = e
	hasComments := len(p.comments) > 0
	rb.mu.Unlock()

	// If comments already arrived, reset timer to a short window
	// to catch any remaining stragglers.
	if hasComments {
		p.timer.Reset(500 * time.Millisecond)
	}
}

// addComment buffers a review comment event. May trigger a flush.
func (rb *reviewBuffer) addComment(e *gh.PullRequestReviewCommentEvent) {
	reviewID := e.GetComment().GetPullRequestReviewID()

	rb.mu.Lock()
	p, ok := rb.pending[reviewID]
	if !ok {
		p = &pendingReview{}
		rb.pending[reviewID] = p
		p.timer = time.AfterFunc(reviewBufferTimeout, func() {
			rb.flush(reviewID)
		})
	}
	p.comments = append(p.comments, e)
	// Reset timer on each new comment to wait for more
	p.timer.Reset(reviewBufferTimeout)
	rb.mu.Unlock()
}

// take removes and returns the pending review for the given ID.
// Returns nil if not found (already flushed).
func (rb *reviewBuffer) take(reviewID int64) *pendingReview {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	p, ok := rb.pending[reviewID]
	if !ok {
		return nil
	}
	delete(rb.pending, reviewID)
	return p
}
