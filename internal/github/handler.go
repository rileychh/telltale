package github

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"

	gh "github.com/google/go-github/v69/github"
	"github.com/rileychh/telltale/internal/store"
	"github.com/rileychh/telltale/internal/telegram"
)

var reImagePlaceholder = regexp.MustCompile(`\[Image #\d+\]`)

type Handler struct {
	secret  []byte
	tg      *telegram.Bot
	db      *store.Store
	reviews *reviewBuffer
}

func NewHandler(secret string, tg *telegram.Bot, db *store.Store) *Handler {
	h := &Handler{
		secret: []byte(secret),
		tg:     tg,
		db:     db,
	}
	h.reviews = newReviewBuffer(func(reviewID int64) {
		h.flushReview(reviewID)
	})
	return h
}

// send sends an HTML message, using a photo or media group when images are present.
func (h *Handler) send(ctx context.Context, html string, imageURLs []string) (int, error) {
	if len(imageURLs) == 0 {
		return h.tg.Send(ctx, html)
	}
	msgID, err := h.tg.SendPhotos(ctx, imageURLs, html)
	if err != nil {
		log.Printf("failed to send photos, falling back to text: %v", err)
		return h.tg.Send(ctx, html)
	}
	return msgID, nil
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
	case *gh.PullRequestReviewCommentEvent:
		h.handlePullRequestReviewComment(ctx, e)
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
	log.Printf("sent issue notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, issue.GetNumber(), false, 0, "", false); err != nil {
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
	log.Printf("sent PR notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, "", false); err != nil {
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
	log.Printf("sent comment notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, issue.GetNumber(), issue.IsPullRequest(), comment.GetID(), "", false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

func (h *Handler) handlePullRequestReview(_ context.Context, e *gh.PullRequestReviewEvent) {
	if e.GetAction() != "submitted" {
		return
	}
	review := e.GetReview()
	switch review.GetState() {
	case "approved", "changes_requested", "commented":
	default:
		return
	}
	h.reviews.addReview(e)
}

func (h *Handler) handlePullRequestReviewComment(_ context.Context, e *gh.PullRequestReviewCommentEvent) {
	if e.GetAction() != "created" {
		return
	}
	if e.GetComment().GetUser().GetType() == "Bot" {
		return
	}
	h.reviews.addComment(e)
}

// flushReview consolidates buffered review events into Telegram messages.
func (h *Handler) flushReview(reviewID int64) {
	p := h.reviews.take(reviewID)
	if p == nil {
		return
	}

	// Standalone single comment or reply (not part of a formal review submission)
	if p.review != nil && p.review.GetReview().GetState() == "commented" &&
		p.review.GetReview().GetBody() == "" && len(p.comments) == 1 {
		h.sendSingleReviewComment(p.comments[0])
		return
	}

	// No review event arrived (timeout without it) — send comments individually
	if p.review == nil {
		for _, ce := range p.comments {
			h.sendSingleReviewComment(ce)
		}
		return
	}

	h.sendConsolidatedReview(p)
}

func (h *Handler) sendConsolidatedReview(p *pendingReview) {
	ctx := context.Background()
	review := p.review.GetReview()
	pr := p.review.GetPullRequest()
	repo := p.review.GetRepo().GetFullName()

	reviewer := escapeHTML(review.GetUser().GetLogin())
	var header string
	switch review.GetState() {
	case "approved":
		header = "✅ <b>Approved by " + reviewer + "</b>"
	case "changes_requested":
		header = "🛑 <b>Changes Requested by " + reviewer + "</b>"
	case "commented":
		header = "👀 <b>Reviewed by " + reviewer + "</b>"
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

	if len(p.comments) > 0 {
		html += fmt.Sprintf("\n\n── %d inline comments ──", len(p.comments))
		const maxLen = 4000
		shown := 0
		for _, ce := range p.comments {
			comment := ce.GetComment()
			location := formatCommentLocation(comment)
			entry := fmt.Sprintf("\n\n📝 <code>%s</code>", escapeHTML(location))
			if body := comment.GetBody(); body != "" {
				converted, imgs := mdToTelegramHTML(body, repo)
				entry += "\n" + converted
				imageURLs = append(imageURLs, imgs...)
			}
			if len([]rune(html))+len([]rune(entry)) > maxLen {
				remaining := len(p.comments) - shown
				html += fmt.Sprintf("\n\n… and <a href=\"%s\">%d more</a>",
					review.GetHTMLURL(), remaining)
				break
			}
			html += entry
			shown++
		}
	}

	// Renumber image placeholders sequentially across all sections
	imgNum := 0
	html = reImagePlaceholder.ReplaceAllStringFunc(html, func(_ string) string {
		imgNum++
		return "[Image #" + strconv.Itoa(imgNum) + "]"
	})

	msgID, err := h.send(ctx, html, imageURLs)
	if err != nil {
		log.Printf("failed to send consolidated review for %s#%d: %v", repo, pr.GetNumber(), err)
		return
	}
	log.Printf("sent consolidated review for %s#%d (%d comments, msg %d)", repo, pr.GetNumber(), len(p.comments), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, review.GetBody(), false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

func (h *Handler) sendSingleReviewComment(e *gh.PullRequestReviewCommentEvent) {
	ctx := context.Background()
	comment := e.GetComment()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	user := escapeHTML(comment.GetUser().GetLogin())
	var header string
	if comment.GetInReplyTo() > 0 {
		header = "💬 <b>Reply by " + user + "</b>"
	} else {
		header = "💬 <b>Review comment by " + user + "</b>"
	}

	location := formatCommentLocation(comment)
	html := fmt.Sprintf(
		`%s`+"\n"+`<a href="%s">%s#%d</a>: %s`+"\n"+`On <code>%s</code>:`,
		header,
		comment.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
		escapeHTML(location),
	)

	var imageURLs []string
	if body := comment.GetBody(); body != "" {
		converted, imgs := mdToTelegramHTML(body, repo)
		html += "\n\n" + converted
		imageURLs = imgs
	}

	msgID, err := h.send(ctx, html, imageURLs)
	if err != nil {
		log.Printf("failed to send review comment notification: %v", err)
		return
	}
	log.Printf("sent review comment notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, comment.GetID(), "", true); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

func formatCommentLocation(comment *gh.PullRequestComment) string {
	sidePrefix := func(side string) string {
		if side == "LEFT" {
			return "L"
		}
		return "R"
	}
	switch {
	case comment.GetSubjectType() == "file" || comment.GetLine() == 0:
		return comment.GetPath()
	case comment.GetStartLine() > 0 && comment.GetStartLine() != comment.GetLine():
		return fmt.Sprintf("%s:%s%d-%s%d", comment.GetPath(),
			sidePrefix(comment.GetStartSide()), comment.GetStartLine(),
			sidePrefix(comment.GetSide()), comment.GetLine())
	default:
		return fmt.Sprintf("%s:%s%d", comment.GetPath(),
			sidePrefix(comment.GetSide()), comment.GetLine())
	}
}
