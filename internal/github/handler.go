package github

import (
	"context"
	"fmt"
	"log"
	"net/http"

	gh "github.com/google/go-github/v69/github"
	"github.com/rileychh/telltale/internal/store"
	"github.com/rileychh/telltale/internal/telegram"
)

type Handler struct {
	secret []byte
	tg     *telegram.Bot
	db     *store.Store
}

func NewHandler(secret string, tg *telegram.Bot, db *store.Store) *Handler {
	return &Handler{
		secret: []byte(secret),
		tg:     tg,
		db:     db,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload, err := gh.ValidatePayload(r, h.secret)
	if err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	event, err := gh.ParseWebHook(gh.WebHookType(r), payload)
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	switch e := event.(type) {
	case *gh.IssuesEvent:
		h.handleIssue(ctx, e)
	case *gh.PullRequestEvent:
		h.handlePullRequest(ctx, e)
	case *gh.IssueCommentEvent:
		h.handleIssueComment(ctx, e)
	case *gh.PullRequestReviewEvent:
		h.handlePullRequestReview(ctx, e)
	}

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleIssue(ctx context.Context, e *gh.IssuesEvent) {
	action := e.GetAction()
	issue := e.GetIssue()
	repo := e.GetRepo().GetFullName()

	var emoji string
	switch action {
	case "opened":
		emoji = "🟢"
	case "closed":
		switch issue.GetStateReason() {
		case "not_planned":
			emoji = "⚪"
			action = "closed as not planned"
		default:
			emoji = "🟣"
			action = "closed as completed"
		}
	case "reopened":
		emoji = "🟢"
	default:
		return
	}

	html := fmt.Sprintf(
		`%s <b>Issue %s</b>`+"\n"+`<a href="%s">%s#%d</a>: %s`+"\n"+`by %s`,
		emoji, action,
		issue.GetHTMLURL(), repo, issue.GetNumber(), escapeHTML(issue.GetTitle()),
		escapeHTML(issue.GetUser().GetLogin()),
	)

	if body := issue.GetBody(); body != "" {
		html += "\n\n" + mdToTelegramHTML(body, repo)
	}

	msgID, err := h.tg.Send(ctx, html)
	if err != nil {
		log.Printf("failed to send issue notification: %v", err)
		return
	}

	if err := h.db.Save(msgID, repo, issue.GetNumber(), false, 0); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

func (h *Handler) handlePullRequest(ctx context.Context, e *gh.PullRequestEvent) {
	action := e.GetAction()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	var emoji string
	switch action {
	case "opened":
		if pr.GetDraft() {
			emoji = "⚪"
			action = "drafted"
		} else {
			emoji = "🟢"
		}
	case "closed":
		if pr.GetMerged() {
			emoji = "🟣"
			action = "merged"
		} else {
			emoji = "🔴"
		}
	case "reopened":
		emoji = "🟢"
	case "ready_for_review":
		emoji = "👀"
	case "converted_to_draft":
		emoji = "⚪"
		action = "converted to draft"
	default:
		return
	}

	html := fmt.Sprintf(
		`%s <b>PR %s</b>`+"\n"+`<a href="%s">%s#%d</a>: %s`+"\n"+`by %s`,
		emoji, action,
		pr.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
		escapeHTML(pr.GetUser().GetLogin()),
	)

	if body := pr.GetBody(); body != "" {
		html += "\n\n" + mdToTelegramHTML(body, repo)
	}

	msgID, err := h.tg.Send(ctx, html)
	if err != nil {
		log.Printf("failed to send PR notification: %v", err)
		return
	}

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

func (h *Handler) handleIssueComment(ctx context.Context, e *gh.IssueCommentEvent) {
	if e.GetAction() != "created" {
		return
	}

	comment := e.GetComment()
	if comment.GetUser().GetType() == "Bot" {
		return
	}

	issue := e.GetIssue()
	repo := e.GetRepo().GetFullName()

	kind := "Issue"
	if issue.IsPullRequest() {
		kind = "PR"
	}

	html := fmt.Sprintf(
		`💬 <b>Comment on %s</b>`+"\n"+`<a href="%s">%s#%d</a>: %s`+"\n"+`%s:`,
		kind,
		comment.GetHTMLURL(), repo, issue.GetNumber(), escapeHTML(issue.GetTitle()),
		escapeHTML(comment.GetUser().GetLogin()),
	)

	if body := comment.GetBody(); body != "" {
		html += "\n\n" + mdToTelegramHTML(body, repo)
	}

	msgID, err := h.tg.Send(ctx, html)
	if err != nil {
		log.Printf("failed to send comment notification: %v", err)
		return
	}

	if err := h.db.Save(msgID, repo, issue.GetNumber(), issue.IsPullRequest(), comment.GetID()); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

func (h *Handler) handlePullRequestReview(ctx context.Context, e *gh.PullRequestReviewEvent) {
	if e.GetAction() != "submitted" {
		return
	}

	review := e.GetReview()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	var emoji string
	switch review.GetState() {
	case "approved":
		emoji = "✅"
	case "changes_requested":
		emoji = "🔄"
	case "commented":
		return // avoid duplicate with issue_comment
	default:
		return
	}

	html := fmt.Sprintf(
		`%s <b>Review: %s</b>`+"\n"+`<a href="%s">%s#%d</a>: %s`+"\n"+`by %s`,
		emoji, review.GetState(),
		review.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
		escapeHTML(review.GetUser().GetLogin()),
	)

	if _, err := h.tg.Send(ctx, html); err != nil {
		log.Printf("failed to send review notification: %v", err)
	}
}
