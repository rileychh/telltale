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

// send sends an HTML message, using a photo or media group when images are present.
func (h *Handler) send(ctx context.Context, html string, imageURLs []string) (int, error) {
	if len(imageURLs) == 0 {
		return h.tg.Send(ctx, html)
	}
	return h.tg.SendPhotos(ctx, imageURLs, html)
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

	user := escapeHTML(e.GetSender().GetLogin())
	var header string
	switch action {
	case "opened":
		header = "🟢 <b>Issue opened by " + user + "</b>"
	case "closed":
		switch issue.GetStateReason() {
		case "not_planned":
			header = "⚪ <b>Issue closed as not planned by " + user + "</b>"
		default:
			header = "🟣 <b>Issue closed as completed by " + user + "</b>"
		}
	case "reopened":
		header = "🟢 <b>Issue reopened by " + user + "</b>"
	default:
		return
	}

	html := fmt.Sprintf(
		`%s`+"\n"+`<a href="%s">%s#%d</a>: %s`,
		header,
		issue.GetHTMLURL(), repo, issue.GetNumber(), escapeHTML(issue.GetTitle()),
	)

	var imageURLs []string
	if action == "opened" {
		if body := issue.GetBody(); body != "" {
			converted, imgs := mdToTelegramHTML(body, repo)
			html += "\n\n" + converted
			imageURLs = imgs
		}
	}

	msgID, err := h.send(ctx, html, imageURLs)
	if err != nil {
		log.Printf("failed to send issue notification: %v", err)
		return
	}

	if err := h.db.Save(msgID, repo, issue.GetNumber(), false, 0, ""); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

func (h *Handler) handlePullRequest(ctx context.Context, e *gh.PullRequestEvent) {
	action := e.GetAction()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	user := escapeHTML(e.GetSender().GetLogin())
	var header string
	switch action {
	case "opened":
		if pr.GetDraft() {
			header = "⚪ <b>PR drafted by " + user + "</b>"
		} else {
			header = "🟢 <b>PR opened by " + user + "</b>"
		}
	case "closed":
		if pr.GetMerged() {
			header = "🟣 <b>PR merged by " + user + "</b>"
		} else {
			header = "🔴 <b>PR closed by " + user + "</b>"
		}
	case "reopened":
		header = "🟢 <b>PR reopened by " + user + "</b>"
	case "ready_for_review":
		header = "👀 <b>PR ready for review by " + user + "</b>"
	case "converted_to_draft":
		header = "⚪ <b>PR converted to draft by " + user + "</b>"
	default:
		return
	}

	html := fmt.Sprintf(
		`%s`+"\n"+`<a href="%s">%s#%d</a>: %s`,
		header,
		pr.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
	)

	var imageURLs []string
	if action == "opened" {
		if body := pr.GetBody(); body != "" {
			converted, imgs := mdToTelegramHTML(body, repo)
			html += "\n\n" + converted
			imageURLs = imgs
		}
	}

	msgID, err := h.send(ctx, html, imageURLs)
	if err != nil {
		log.Printf("failed to send PR notification: %v", err)
		return
	}

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, ""); err != nil {
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

	user := escapeHTML(comment.GetUser().GetLogin())
	html := fmt.Sprintf(
		`💬 <b>Comment on %s by %s</b>`+"\n"+`<a href="%s">%s#%d</a>: %s`,
		kind, user,
		comment.GetHTMLURL(), repo, issue.GetNumber(), escapeHTML(issue.GetTitle()),
	)

	var imageURLs []string
	if body := comment.GetBody(); body != "" {
		converted, imgs := mdToTelegramHTML(body, repo)
		html += "\n\n" + converted
		imageURLs = imgs
	}

	msgID, err := h.send(ctx, html, imageURLs)
	if err != nil {
		log.Printf("failed to send comment notification: %v", err)
		return
	}

	if err := h.db.Save(msgID, repo, issue.GetNumber(), issue.IsPullRequest(), comment.GetID(), ""); err != nil {
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

	reviewer := escapeHTML(review.GetUser().GetLogin())
	var header string
	switch review.GetState() {
	case "approved":
		header = "✅ <b>Approved by " + reviewer + "</b>"
	case "changes_requested":
		header = "🛑 <b>Changes Requested by " + reviewer + "</b>"
	case "commented":
		header = "👀 <b>Reviewed by " + reviewer + "</b>"
	default:
		return
	}

	html := fmt.Sprintf(
		`%s`+"\n"+`<a href="%s">%s#%d</a>: %s`,
		header,
		review.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
	)

	var imageURLs []string
	if body := review.GetBody(); body != "" && review.GetUser().GetType() != "Bot" {
		converted, imgs := mdToTelegramHTML(body, repo)
		html += "\n\n" + converted
		imageURLs = imgs
	}

	msgID, err := h.send(ctx, html, imageURLs)
	if err != nil {
		log.Printf("failed to send review notification: %v", err)
		return
	}

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, review.GetBody()); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}
