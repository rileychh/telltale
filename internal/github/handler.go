package github

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v69/github"
	"github.com/rileychh/telltale/internal/store"
	"github.com/rileychh/telltale/internal/tableimg"
	"github.com/rileychh/telltale/internal/telegram"
)

var reMediaPlaceholder = regexp.MustCompile(`\[(Image|Table) #\d+\]`)

type Handler struct {
	secret  []byte
	tg      *telegram.Bot
	db      *store.Store
	gh      *Client
	reviews *reviewBuffer
}

func NewHandler(secret string, tg *telegram.Bot, db *store.Store, gh *Client) *Handler {
	h := &Handler{
		secret: []byte(secret),
		tg:     tg,
		db:     db,
		gh:     gh,
	}
	h.reviews = newReviewBuffer(func(reviewID int64) {
		h.flushReview(reviewID)
	})
	return h
}

// send sends an HTML message, using a photo or media group when media is present.
// If replyTo > 0, the message is sent as a reply to that Telegram message ID.
func (h *Handler) send(ctx context.Context, html string, refs []MediaRef, replyTo int) (int, error) {
	media := resolveMedia(refs)
	if len(media) == 0 {
		return h.tg.Send(ctx, html, replyTo)
	}
	msgID, err := h.tg.SendMedia(ctx, media, html, replyTo)
	if err != nil {
		log.Printf("failed to send media, falling back to text: %v", err)
		return h.tg.Send(ctx, html, replyTo)
	}
	return msgID, nil
}

// sendThreaded sends a notification that replies to the latest prior
// notification for this issue/PR (if any), then records the new message as
// the latest for future notifications.
func (h *Handler) sendThreaded(ctx context.Context, repo string, issueNumber int, html string, refs []MediaRef) (int, error) {
	replyTo, err := h.db.LookupLatest(repo, issueNumber)
	if err != nil {
		log.Printf("failed to look up latest message for %s#%d: %v", repo, issueNumber, err)
	}
	msgID, err := h.send(ctx, html, refs, replyTo)
	if err != nil {
		return 0, err
	}
	if err := h.db.SaveLatest(repo, issueNumber, msgID); err != nil {
		log.Printf("failed to save latest message for %s#%d: %v", repo, issueNumber, err)
	}
	return msgID, nil
}

// resolveMedia converts MediaRefs to telegram MediaItems, rendering tables to PNG.
func resolveMedia(refs []MediaRef) []telegram.MediaItem {
	var items []telegram.MediaItem
	tableNum := 0
	for _, ref := range refs {
		if ref.URL != "" {
			items = append(items, telegram.MediaItem{URL: ref.URL})
		} else if ref.Table != nil {
			tableNum++
			data, err := tableimg.Render(*ref.Table)
			if err != nil {
				log.Printf("failed to render table: %v", err)
				continue
			}
			items = append(items, telegram.MediaItem{
				Data: data,
				Name: fmt.Sprintf("table_%d.png", tableNum),
			})
		}
	}
	return items
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

	if action == "edited" {
		if e.GetSender().GetType() == "Bot" {
			return
		}
		html, _ := renderIssueOpened(issue, repo)
		h.editEntity(ctx, repo, "issue_body", int64(issue.GetNumber()), html)
		return
	}
	if action == "deleted" {
		h.deleteEntity(ctx, repo, "issue_body", int64(issue.GetNumber()))
		return
	}

	if action == "opened" {
		html, media := renderIssueOpened(issue, repo)
		msgID, err := h.sendThreaded(ctx, repo, issue.GetNumber(), html, media)
		if err != nil {
			log.Printf("failed to send issue notification: %v", err)
			return
		}
		log.Printf("sent issue notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)
		if err := h.db.Save(msgID, repo, issue.GetNumber(), false, 0, "", false); err != nil {
			log.Printf("failed to save message mapping: %v", err)
		}
		if err := h.db.LinkEntity(repo, "issue_body", int64(issue.GetNumber()), msgID, len(media) > 0); err != nil {
			log.Printf("failed to link issue_body: %v", err)
		}
		return
	}

	user := escapeHTML(e.GetSender().GetLogin())
	var header string
	switch action {
	case "closed":
		switch issue.GetStateReason() {
		case "not_planned":
			header = "⚪ <b>Issue closed as not planned by " + user + "</b>"
		default:
			header = "🟣 <b>Issue closed as completed by " + user + "</b>"
		}
	case "reopened":
		header = "🟢 <b>Issue reopened by " + user + "</b>"
	case "assigned":
		assignee := escapeHTML(e.GetAssignee().GetLogin())
		header = "👤 <b>Issue assigned to " + assignee + " by " + user + "</b>"
	default:
		return
	}

	html := fmt.Sprintf(
		`%s`+"\n"+`<a href="%s">%s#%d</a>: %s`,
		header,
		issue.GetHTMLURL(), repo, issue.GetNumber(), escapeHTML(issue.GetTitle()),
	)

	msgID, err := h.sendThreaded(ctx, repo, issue.GetNumber(), html, nil)
	if err != nil {
		log.Printf("failed to send issue notification: %v", err)
		return
	}
	log.Printf("sent issue notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, issue.GetNumber(), false, 0, "", false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

// renderIssueOpened produces the HTML and media for the "Issue opened" message
// in its current state. Used both at opening time and on subsequent body or
// title edits.
func renderIssueOpened(issue *gh.Issue, repo string) (string, []MediaRef) {
	user := escapeHTML(issue.GetUser().GetLogin())
	html := fmt.Sprintf(
		"🟢 <b>Issue opened by %s</b>\n<a href=\"%s\">%s#%d</a>: %s",
		user,
		issue.GetHTMLURL(), repo, issue.GetNumber(), escapeHTML(issue.GetTitle()),
	)
	var media []MediaRef
	if issue.GetUser().GetType() != "Bot" {
		if body := issue.GetBody(); body != "" {
			converted, refs := mdToTelegramHTML(body, repo)
			html += "\n\n" + converted
			media = refs
		}
	}
	return html, media
}

func (h *Handler) handlePullRequest(ctx context.Context, e *gh.PullRequestEvent) {
	action := e.GetAction()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	if action == "edited" {
		if e.GetSender().GetType() == "Bot" {
			return
		}
		html, _ := renderPROpened(pr, repo)
		h.editEntity(ctx, repo, "pr_body", int64(pr.GetNumber()), html)
		return
	}

	if action == "opened" {
		html, media := renderPROpened(pr, repo)
		msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), html, media)
		if err != nil {
			log.Printf("failed to send PR notification: %v", err)
			return
		}
		log.Printf("sent PR notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)
		if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, "", false); err != nil {
			log.Printf("failed to save message mapping: %v", err)
		}
		if err := h.db.LinkEntity(repo, "pr_body", int64(pr.GetNumber()), msgID, len(media) > 0); err != nil {
			log.Printf("failed to link pr_body: %v", err)
		}
		return
	}

	user := escapeHTML(e.GetSender().GetLogin())
	var header string
	switch action {
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
	case "review_requested":
		reviewer := escapeHTML(e.GetRequestedReviewer().GetLogin())
		header = "👀 <b>Review requested from " + reviewer + " by " + user + "</b>"
	default:
		return
	}

	html := fmt.Sprintf(
		`%s`+"\n"+`<a href="%s">%s#%d</a>: %s`,
		header,
		pr.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
	)

	msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), html, nil)
	if err != nil {
		log.Printf("failed to send PR notification: %v", err)
		return
	}
	log.Printf("sent PR notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, "", false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
}

// renderPROpened produces the HTML and media for the "PR opened" or "PR
// drafted" message. Used both at opening time and on subsequent body or title
// edits.
func renderPROpened(pr *gh.PullRequest, repo string) (string, []MediaRef) {
	user := escapeHTML(pr.GetUser().GetLogin())
	var header string
	if pr.GetDraft() {
		header = "⚪ <b>PR drafted by " + user + "</b>"
	} else {
		header = "🟢 <b>PR opened by " + user + "</b>"
	}
	html := fmt.Sprintf(
		"%s\n<a href=\"%s\">%s#%d</a>: %s",
		header,
		pr.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
	)
	var media []MediaRef
	if pr.GetUser().GetType() != "Bot" {
		if body := pr.GetBody(); body != "" {
			converted, refs := mdToTelegramHTML(body, repo)
			html += "\n\n" + converted
			media = refs
		}
	}
	return html, media
}

func (h *Handler) handleIssueComment(ctx context.Context, e *gh.IssueCommentEvent) {
	action := e.GetAction()
	comment := e.GetComment()
	if comment.GetUser().GetType() == "Bot" {
		return
	}

	issue := e.GetIssue()
	repo := e.GetRepo().GetFullName()

	switch action {
	case "edited":
		html, _ := renderIssueComment(issue, comment, repo)
		h.editEntity(ctx, repo, "issue_comment", comment.GetID(), html)
		return
	case "deleted":
		h.deleteEntity(ctx, repo, "issue_comment", comment.GetID())
		return
	case "created":
		// fall through
	default:
		return
	}

	html, media := renderIssueComment(issue, comment, repo)
	msgID, err := h.sendThreaded(ctx, repo, issue.GetNumber(), html, media)
	if err != nil {
		log.Printf("failed to send comment notification: %v", err)
		return
	}
	log.Printf("sent comment notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, issue.GetNumber(), issue.IsPullRequest(), comment.GetID(), "", false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	if err := h.db.LinkEntity(repo, "issue_comment", comment.GetID(), msgID, len(media) > 0); err != nil {
		log.Printf("failed to link issue_comment: %v", err)
	}
}

// renderIssueComment produces the HTML and media for an issue or PR comment
// notification. Used both at creation and on subsequent edits.
func renderIssueComment(issue *gh.Issue, comment *gh.IssueComment, repo string) (string, []MediaRef) {
	kind := "Issue"
	if issue.IsPullRequest() {
		kind = "PR"
	}
	user := escapeHTML(comment.GetUser().GetLogin())
	html := fmt.Sprintf(
		"💬 <b>Comment on %s by %s</b>\n<a href=\"%s\">%s#%d</a>: %s",
		kind, user,
		comment.GetHTMLURL(), repo, issue.GetNumber(), escapeHTML(issue.GetTitle()),
	)
	var media []MediaRef
	if body := comment.GetBody(); body != "" {
		converted, refs := mdToTelegramHTML(body, repo)
		html += "\n\n" + converted
		media = refs
	}
	return html, media
}

func (h *Handler) handlePullRequestReview(ctx context.Context, e *gh.PullRequestReviewEvent) {
	action := e.GetAction()
	review := e.GetReview()
	if review.GetUser().GetType() == "Bot" {
		return
	}

	if action == "edited" {
		repo := e.GetRepo().GetFullName()
		h.refreshConsolidatedReview(ctx, repo, e.GetPullRequest(), review.GetID())
		return
	}

	if action != "submitted" {
		return
	}
	switch review.GetState() {
	case "approved", "changes_requested", "commented":
	default:
		return
	}
	h.reviews.addReview(e)
}

func (h *Handler) handlePullRequestReviewComment(ctx context.Context, e *gh.PullRequestReviewCommentEvent) {
	action := e.GetAction()
	comment := e.GetComment()
	if comment.GetUser().GetType() == "Bot" {
		return
	}
	repo := e.GetRepo().GetFullName()
	commentID := comment.GetID()

	switch action {
	case "edited":
		// Try the single-message path first (sendSingleReviewComment).
		if msgID, hasMedia, err := h.db.LookupEntity(repo, "single_review_comment", commentID); err == nil {
			html, _ := renderSingleReviewComment(e.GetPullRequest(), comment, repo)
			if err := h.tg.EditOrCaption(ctx, msgID, hasMedia, html); err != nil {
				log.Printf("failed to edit review comment %d: %v", commentID, err)
				return
			}
			log.Printf("edited single_review_comment msg %d (%s entity %d)", msgID, repo, commentID)
			return
		}
		// Otherwise it may be part of a consolidated review.
		if _, _, err := h.db.LookupEntity(repo, "consolidated_review_comment", commentID); err == nil {
			h.refreshConsolidatedReview(ctx, repo, e.GetPullRequest(), comment.GetPullRequestReviewID())
		}
		return
	case "deleted":
		if msgID, _, err := h.db.LookupEntity(repo, "single_review_comment", commentID); err == nil {
			if err := h.tg.DeleteMessage(ctx, msgID); err != nil {
				log.Printf("failed to delete telegram message %d: %v", msgID, err)
				return
			}
			if err := h.db.UnlinkAllForMessage(msgID); err != nil {
				log.Printf("failed to unlink message %d: %v", msgID, err)
			}
			log.Printf("deleted single_review_comment msg %d (%s entity %d)", msgID, repo, commentID)
			return
		}
		if _, _, err := h.db.LookupEntity(repo, "consolidated_review_comment", commentID); err == nil {
			// Unlink first so the re-render doesn't re-include this comment
			// if GitHub still returns it transiently.
			if err := h.db.UnlinkEntity(repo, "consolidated_review_comment", commentID); err != nil {
				log.Printf("failed to unlink consolidated_review_comment %d: %v", commentID, err)
			}
			h.refreshConsolidatedReview(ctx, repo, e.GetPullRequest(), comment.GetPullRequestReviewID())
		}
		return
	case "created":
		// fall through to buffering
	default:
		return
	}
	h.reviews.addComment(e)
}

// refreshConsolidatedReview re-fetches a review and its inline comments from
// GitHub, then re-renders the consolidated Telegram message in place.
func (h *Handler) refreshConsolidatedReview(ctx context.Context, repo string, pr *gh.PullRequest, reviewID int64) {
	msgID, hasMedia, err := h.db.LookupEntity(repo, "consolidated_review", reviewID)
	if err != nil {
		return
	}
	review, comments, err := h.gh.GetReviewWithComments(ctx, repo, pr.GetNumber(), reviewID)
	if err != nil {
		log.Printf("failed to fetch review %d for re-render: %v", reviewID, err)
		return
	}
	html, _ := renderConsolidatedReview(review, comments, pr, repo)
	if err := h.tg.EditOrCaption(ctx, msgID, hasMedia, html); err != nil {
		log.Printf("failed to edit consolidated review %d: %v", reviewID, err)
		return
	}
	log.Printf("refreshed consolidated review for %s#%d (review %d, msg %d)", repo, pr.GetNumber(), reviewID, msgID)
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

	comments := make([]*gh.PullRequestComment, len(p.comments))
	for i, ce := range p.comments {
		comments[i] = ce.GetComment()
	}

	html, media := renderConsolidatedReview(review, comments, pr, repo)

	msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), html, media)
	if err != nil {
		log.Printf("failed to send consolidated review for %s#%d: %v", repo, pr.GetNumber(), err)
		return
	}
	log.Printf("sent consolidated review for %s#%d (%d comments, msg %d)", repo, pr.GetNumber(), len(comments), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, review.GetBody(), false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	hasMedia := len(media) > 0
	if err := h.db.LinkEntity(repo, "consolidated_review", review.GetID(), msgID, hasMedia); err != nil {
		log.Printf("failed to link consolidated_review: %v", err)
	}
	for _, c := range comments {
		if err := h.db.LinkEntity(repo, "consolidated_review_comment", c.GetID(), msgID, hasMedia); err != nil {
			log.Printf("failed to link consolidated_review_comment %d: %v", c.GetID(), err)
		}
	}
}

func (h *Handler) sendSingleReviewComment(e *gh.PullRequestReviewCommentEvent) {
	ctx := context.Background()
	comment := e.GetComment()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	html, media := renderSingleReviewComment(pr, comment, repo)

	msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), html, media)
	if err != nil {
		log.Printf("failed to send review comment notification: %v", err)
		return
	}
	log.Printf("sent review comment notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, comment.GetID(), "", true); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	if err := h.db.LinkEntity(repo, "single_review_comment", comment.GetID(), msgID, len(media) > 0); err != nil {
		log.Printf("failed to link single_review_comment: %v", err)
	}
}

// renderSingleReviewComment produces the HTML and media for a single
// review-comment notification (the non-consolidated path).
func renderSingleReviewComment(pr *gh.PullRequest, comment *gh.PullRequestComment, repo string) (string, []MediaRef) {
	user := escapeHTML(comment.GetUser().GetLogin())
	var header string
	if comment.GetInReplyTo() > 0 {
		header = "💬 <b>Reply by " + user + "</b>"
	} else {
		header = "💬 <b>Review comment by " + user + "</b>"
	}

	location := formatCommentLocation(comment)
	html := fmt.Sprintf(
		"%s\n<a href=\"%s\">%s#%d</a>: %s\nOn <code>%s</code>:",
		header,
		comment.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
		escapeHTML(location),
	)

	var media []MediaRef
	if body := comment.GetBody(); body != "" {
		converted, refs := mdToTelegramHTML(body, repo)
		html += "\n\n" + converted
		media = refs
	}
	return html, media
}

// renderConsolidatedReview produces the HTML and media for a consolidated
// review message: the review body + each inline comment, with a "… and N
// more" tail when the rendering would exceed Telegram's text limit. Used both
// at first send and on re-render after edits/deletes.
func renderConsolidatedReview(review *gh.PullRequestReview, comments []*gh.PullRequestComment, pr *gh.PullRequest, repo string) (string, []MediaRef) {
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
		"%s\n<a href=\"%s\">%s#%d</a>: %s",
		header,
		review.GetHTMLURL(), repo, pr.GetNumber(), escapeHTML(pr.GetTitle()),
	)

	var media []MediaRef
	if body := review.GetBody(); body != "" && review.GetUser().GetType() != "Bot" {
		converted, refs := mdToTelegramHTML(body, repo)
		html += "\n\n" + converted
		media = refs
	}

	if len(comments) > 0 {
		html += fmt.Sprintf("\n\n── %d inline comments ──", len(comments))
		const maxLen = 4000
		shown := 0
		for _, comment := range comments {
			location := formatCommentLocation(comment)
			entry := fmt.Sprintf("\n\n📝 <code>%s</code>", escapeHTML(location))
			if body := comment.GetBody(); body != "" {
				converted, refs := mdToTelegramHTML(body, repo)
				entry += "\n" + converted
				media = append(media, refs...)
			}
			if len([]rune(html))+len([]rune(entry)) > maxLen {
				remaining := len(comments) - shown
				html += fmt.Sprintf("\n\n… and <a href=\"%s\">%d more</a>",
					review.GetHTMLURL(), remaining)
				break
			}
			html += entry
			shown++
		}
	}

	// Renumber media placeholders sequentially across all sections.
	imgNum := 0
	html = reMediaPlaceholder.ReplaceAllStringFunc(html, func(match string) string {
		imgNum++
		kind := "Image"
		if strings.Contains(match, "Table") {
			kind = "Table"
		}
		return "[" + kind + " #" + strconv.Itoa(imgNum) + "]"
	})

	return html, media
}

// editEntity edits the Telegram message linked to a GitHub entity in place.
// No-ops if the entity has never been linked.
func (h *Handler) editEntity(ctx context.Context, repo, entityType string, entityID int64, html string) {
	msgID, hasMedia, err := h.db.LookupEntity(repo, entityType, entityID)
	if err != nil {
		// sql.ErrNoRows is the common case; ignore.
		return
	}
	if err := h.tg.EditOrCaption(ctx, msgID, hasMedia, html); err != nil {
		log.Printf("failed to edit %s msg %d: %v", entityType, msgID, err)
		return
	}
	log.Printf("edited %s msg %d (%s entity %d)", entityType, msgID, repo, entityID)
}

// deleteEntity removes the Telegram message linked to a GitHub entity and
// drops all entity_index entries pointing at that message.
func (h *Handler) deleteEntity(ctx context.Context, repo, entityType string, entityID int64) {
	msgID, _, err := h.db.LookupEntity(repo, entityType, entityID)
	if err != nil {
		return
	}
	if err := h.tg.DeleteMessage(ctx, msgID); err != nil {
		log.Printf("failed to delete %s msg %d: %v", entityType, msgID, err)
		return
	}
	if err := h.db.UnlinkAllForMessage(msgID); err != nil {
		log.Printf("failed to unlink message %d: %v", msgID, err)
	}
	log.Printf("deleted %s msg %d (%s entity %d)", entityType, msgID, repo, entityID)
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
