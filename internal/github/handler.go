package github

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v69/github"
	"github.com/rileychh/telltale/internal/store"
	"github.com/rileychh/telltale/internal/telegram"
)

type Handler struct {
	secret         []byte
	allowedRepos   map[string]bool
	tg             *telegram.Bot
	db             *store.Store
	gh             *Client
	reviews        *reviewBuffer
	closures       *closeBuffer
	reviewRequests *eventBurstBuffer[*gh.PullRequestEvent]
	assignments    *eventBurstBuffer[*gh.IssuesEvent]
}

// repoEvent is implemented by every webhook event type we dispatch on. All
// go-github webhook events that carry a repository expose GetRepo().
type repoEvent interface {
	GetRepo() *gh.Repository
}

func NewHandler(secret string, allowedRepos []string, tg *telegram.Bot, db *store.Store, githubClient *Client) *Handler {
	var allow map[string]bool
	if len(allowedRepos) > 0 {
		allow = make(map[string]bool, len(allowedRepos))
		for _, r := range allowedRepos {
			allow[r] = true
		}
	}
	h := &Handler{
		secret:       []byte(secret),
		allowedRepos: allow,
		tg:           tg,
		db:           db,
		gh:           githubClient,
	}
	h.reviews = newReviewBuffer(func(reviewID int64) {
		h.flushReview(reviewID)
	})
	h.closures = newCloseBuffer(func(key closeKey) {
		h.flushClose(key)
	})
	h.reviewRequests = newEventBurstBuffer[*gh.PullRequestEvent](func(key eventBurstKey) {
		h.flushReviewRequests(key)
	})
	h.assignments = newEventBurstBuffer[*gh.IssuesEvent](func(key eventBurstKey) {
		h.flushAssignments(key)
	})
	return h
}

type notificationSender interface {
	SendRich(context.Context, string, int) (int, error)
	Send(context.Context, string, int) (int, error)
}

// send sends a rich Markdown message. If Telegram rejects a media attachment,
// it retries the rich message with images demoted to links. Malformed Markdown,
// excessive block counts, and other rich-message failures still fall back to a
// plain text message so the notification arrives.
// If replyTo > 0, the message is sent as a reply to that Telegram message ID.
func (h *Handler) send(ctx context.Context, md string, replyTo int) (int, error) {
	return sendNotification(ctx, h.tg, md, replyTo)
}

func sendNotification(ctx context.Context, sender notificationSender, md string, replyTo int) (int, error) {
	msgID, err := sender.SendRich(ctx, md, replyTo)
	if err == nil {
		return msgID, nil
	}

	withoutMedia := demoteImages(md)
	if withoutMedia != md {
		log.Printf("failed to send rich message, retrying without media: %v", err)
		if msgID, retryErr := sender.SendRich(ctx, withoutMedia, replyTo); retryErr == nil {
			return msgID, nil
		} else {
			err = retryErr
		}
	}

	log.Printf("failed to send rich message, falling back to text: %v", err)
	return sender.Send(ctx, escapeHTML(md), replyTo)
}

// sendThreaded sends a notification that replies to the latest prior
// notification for this issue/PR (if any), then records the new message as
// the latest for future notifications.
func (h *Handler) sendThreaded(ctx context.Context, repo string, issueNumber int, md string) (int, error) {
	replyTo, err := h.db.LookupLatest(repo, issueNumber)
	if err != nil {
		log.Printf("failed to look up latest message for %s#%d: %v", repo, issueNumber, err)
	}
	msgID, err := h.send(ctx, md, replyTo)
	if err != nil {
		return 0, err
	}
	if err := h.db.SaveLatest(repo, issueNumber, msgID); err != nil {
		log.Printf("failed to save latest message for %s#%d: %v", repo, issueNumber, err)
	}
	return msgID, nil
}

// escapeHTML escapes text for the plain-text fallback path, which still uses
// Telegram's regular HTML parse mode.
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
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

	if h.allowedRepos != nil {
		var repo string
		if re, ok := event.(repoEvent); ok {
			repo = re.GetRepo().GetFullName()
		}
		if !h.allowedRepos[repo] {
			log.Printf("dropped webhook from disallowed repo %q", repo)
			w.WriteHeader(http.StatusOK)
			return
		}
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
		h.editEntity(ctx, repo, "issue_body", int64(issue.GetNumber()), renderIssueOpened(issue, repo))
		return
	}
	if action == "deleted" {
		h.deleteEntity(ctx, repo, "issue_body", int64(issue.GetNumber()))
		return
	}

	if action == "opened" {
		msgID, err := h.sendThreaded(ctx, repo, issue.GetNumber(), renderIssueOpened(issue, repo))
		if err != nil {
			log.Printf("failed to send issue notification: %v", err)
			return
		}
		log.Printf("sent issue notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)
		if err := h.db.Save(msgID, repo, issue.GetNumber(), false, 0, "", false); err != nil {
			log.Printf("failed to save message mapping: %v", err)
		}
		if err := h.db.LinkEntity(repo, "issue_body", int64(issue.GetNumber()), msgID); err != nil {
			log.Printf("failed to link issue_body: %v", err)
		}
		return
	}
	if action == "closed" {
		h.closures.addIssue(e)
		return
	}
	if action == "assigned" {
		h.assignments.add(eventBurstKey{
			repo:   repo,
			number: issue.GetNumber(),
			actor:  e.GetSender().GetLogin(),
		}, e)
		return
	}

	md := renderIssueLifecycle(issue, repo, action, e.GetSender().GetLogin())
	if md == "" {
		return
	}
	h.sendIssueLifecycle(ctx, repo, issue, md, 0)
}

func renderIssueLifecycle(issue *gh.Issue, repo, action, actor string) string {
	user := escapeMarkdown(actor)
	var header string
	switch action {
	case "closed":
		switch issue.GetStateReason() {
		case "not_planned":
			header = "⚪ **Issue closed as not planned by " + user + "**"
		default:
			header = "🟣 **Issue closed as completed by " + user + "**"
		}
	case "reopened":
		header = "🟢 **Issue reopened by " + user + "**"
	default:
		return ""
	}

	return fmt.Sprintf(
		"%s\n[%s#%d](%s): %s",
		header,
		repo, issue.GetNumber(), issue.GetHTMLURL(), escapeMarkdown(issue.GetTitle()),
	)
}

func renderIssueClose(issue *gh.Issue, repo, actor, commentBody string) string {
	md := renderIssueLifecycle(issue, repo, "closed", actor)
	if commentBody != "" {
		md += "\n\n" + prepareMarkdown(commentBody, repo)
	}
	return md
}

func (h *Handler) sendIssueLifecycle(ctx context.Context, repo string, issue *gh.Issue, md string, commentID int64) {
	msgID, err := h.sendThreaded(ctx, repo, issue.GetNumber(), md)
	if err != nil {
		log.Printf("failed to send issue notification: %v", err)
		return
	}
	log.Printf("sent issue notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, issue.GetNumber(), false, commentID, "", false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	if commentID != 0 {
		if err := h.db.LinkEntity(repo, "issue_closing_comment", commentID, msgID); err != nil {
			log.Printf("failed to link issue_closing_comment: %v", err)
		}
	}
}
func (h *Handler) flushAssignments(key eventBurstKey) {
	events := h.assignments.take(key)
	if len(events) == 0 {
		return
	}
	md := renderAssignments(events, key)
	if md == "" {
		return
	}
	h.sendIssueLifecycle(context.Background(), key.repo, events[0].GetIssue(), md, 0)
}

func renderAssignments(events []*gh.IssuesEvent, key eventBurstKey) string {
	assignees := make([]string, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		assignee := event.GetAssignee().GetLogin()
		if assignee == "" || seen[assignee] {
			continue
		}
		seen[assignee] = true
		assignees = append(assignees, escapeMarkdown(assignee))
	}
	if len(assignees) == 0 {
		return ""
	}

	issue := events[0].GetIssue()
	return fmt.Sprintf(
		"👤 **Issue assigned to %s by %s**\n[%s#%d](%s): %s",
		formatHumanList(assignees), escapeMarkdown(key.actor),
		key.repo, issue.GetNumber(), issue.GetHTMLURL(), escapeMarkdown(issue.GetTitle()),
	)
}

func (h *Handler) flushClose(key closeKey) {
	p := h.closures.take(key)
	if p == nil {
		return
	}

	var commentID int64
	var commentBody string
	if p.comment != nil {
		commentID = p.comment.GetComment().GetID()
		commentBody = p.comment.GetComment().GetBody()
	}

	switch {
	case p.issue != nil:
		issue := p.issue.GetIssue()
		h.sendIssueLifecycle(
			context.Background(),
			key.repo,
			issue,
			renderIssueClose(issue, key.repo, p.issue.GetSender().GetLogin(), commentBody),
			commentID,
		)
	case p.pullRequest != nil:
		pr := p.pullRequest.GetPullRequest()
		h.sendPRLifecycle(
			context.Background(),
			key.repo,
			pr,
			renderPRClose(pr, key.repo, p.pullRequest.GetSender().GetLogin(), commentBody),
			commentID,
		)
	}
}

// renderIssueOpened produces the Markdown for the "Issue opened" message in its
// current state. Used both at opening time and on subsequent body or title edits.
func renderIssueOpened(issue *gh.Issue, repo string) string {
	md := fmt.Sprintf(
		"🟢 **Issue opened by %s**\n[%s#%d](%s): %s",
		escapeMarkdown(issue.GetUser().GetLogin()),
		repo, issue.GetNumber(), issue.GetHTMLURL(), escapeMarkdown(issue.GetTitle()),
	)
	if issue.GetUser().GetType() != "Bot" {
		if body := issue.GetBody(); body != "" {
			md += "\n\n" + prepareMarkdown(body, repo)
		}
	}
	return md
}

func (h *Handler) handlePullRequest(ctx context.Context, e *gh.PullRequestEvent) {
	action := e.GetAction()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	if action == "edited" {
		if e.GetSender().GetType() == "Bot" {
			return
		}
		h.editEntity(ctx, repo, "pr_body", int64(pr.GetNumber()), renderPROpened(pr, repo))
		return
	}

	if action == "opened" {
		msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), renderPROpened(pr, repo))
		if err != nil {
			log.Printf("failed to send PR notification: %v", err)
			return
		}
		log.Printf("sent PR notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)
		if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, "", false); err != nil {
			log.Printf("failed to save message mapping: %v", err)
		}
		if err := h.db.LinkEntity(repo, "pr_body", int64(pr.GetNumber()), msgID); err != nil {
			log.Printf("failed to link pr_body: %v", err)
		}
		return
	}
	if action == "closed" {
		h.closures.addPullRequest(e)
		return
	}
	if action == "review_requested" {
		if _, ok := reviewRequestTarget(e); !ok {
			return
		}
		h.reviewRequests.add(eventBurstKey{
			repo:   repo,
			number: pr.GetNumber(),
			actor:  e.GetSender().GetLogin(),
		}, e)
		return
	}

	user := escapeMarkdown(e.GetSender().GetLogin())
	var header string
	switch action {
	case "reopened":
		header = "🟢 **PR reopened by " + user + "**"
	case "ready_for_review":
		header = "👀 **PR ready for review by " + user + "**"
	case "converted_to_draft":
		header = "⚪ **PR converted to draft by " + user + "**"
	default:
		return
	}

	md := fmt.Sprintf(
		"%s\n[%s#%d](%s): %s",
		header,
		repo, pr.GetNumber(), pr.GetHTMLURL(), escapeMarkdown(pr.GetTitle()),
	)

	h.sendPRLifecycle(ctx, repo, pr, md, 0)
}

func renderPRClose(pr *gh.PullRequest, repo, actor, commentBody string) string {
	return renderPRCloseMessage(
		pr.GetNumber(),
		pr.GetTitle(),
		pr.GetHTMLURL(),
		repo,
		actor,
		pr.GetMerged(),
		commentBody,
	)
}

func renderPRCloseFromIssue(issue *gh.Issue, repo, actor, commentBody string) string {
	links := issue.GetPullRequestLinks()
	return renderPRCloseMessage(
		issue.GetNumber(),
		issue.GetTitle(),
		issue.GetHTMLURL(),
		repo,
		actor,
		links != nil && links.MergedAt != nil,
		commentBody,
	)
}

func renderPRCloseMessage(number int, title, url, repo, actor string, merged bool, commentBody string) string {
	action := "closed"
	icon := "🔴"
	if merged {
		action = "merged"
		icon = "🟣"
	}
	md := fmt.Sprintf(
		"%s **PR %s by %s**\n[%s#%d](%s): %s",
		icon, action, escapeMarkdown(actor),
		repo, number, url, escapeMarkdown(title),
	)
	if commentBody != "" {
		md += "\n\n" + prepareMarkdown(commentBody, repo)
	}
	return md
}

func (h *Handler) sendPRLifecycle(ctx context.Context, repo string, pr *gh.PullRequest, md string, commentID int64) {
	msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), md)
	if err != nil {
		log.Printf("failed to send PR notification: %v", err)
		return
	}
	log.Printf("sent PR notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, commentID, "", false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	if commentID != 0 {
		if err := h.db.LinkEntity(repo, "pr_closing_comment", commentID, msgID); err != nil {
			log.Printf("failed to link pr_closing_comment: %v", err)
		}
	}
}
func reviewRequestTarget(e *gh.PullRequestEvent) (string, bool) {
	if reviewer := e.GetRequestedReviewer(); reviewer != nil {
		if reviewer.GetType() == "Bot" {
			return "", false
		}
		return reviewer.GetLogin(), reviewer.GetLogin() != ""
	}
	if team := e.GetRequestedTeam(); team != nil {
		name := team.GetName()
		if name == "" {
			name = team.GetSlug()
		}
		return name, name != ""
	}
	return "", false
}

func (h *Handler) flushReviewRequests(key eventBurstKey) {
	events := h.reviewRequests.take(key)
	if len(events) == 0 {
		return
	}
	md := renderReviewRequests(events, key)
	if md == "" {
		return
	}
	h.sendPRLifecycle(context.Background(), key.repo, events[0].GetPullRequest(), md, 0)
}

func renderReviewRequests(events []*gh.PullRequestEvent, key eventBurstKey) string {
	targets := make([]string, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		target, ok := reviewRequestTarget(event)
		if !ok || seen[target] {
			continue
		}
		seen[target] = true
		targets = append(targets, escapeMarkdown(target))
	}
	if len(targets) == 0 {
		return ""
	}

	pr := events[0].GetPullRequest()
	return fmt.Sprintf(
		"👀 **Review requested from %s by %s**\n[%s#%d](%s): %s",
		formatHumanList(targets), escapeMarkdown(key.actor),
		key.repo, pr.GetNumber(), pr.GetHTMLURL(), escapeMarkdown(pr.GetTitle()),
	)
}

func formatHumanList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

// renderPROpened produces the Markdown for the "PR opened" or "PR drafted"
// message. Used both at opening time and on subsequent body or title edits.
func renderPROpened(pr *gh.PullRequest, repo string) string {
	user := escapeMarkdown(pr.GetUser().GetLogin())
	var header string
	if pr.GetDraft() {
		header = "⚪ **PR drafted by " + user + "**"
	} else {
		header = "🟢 **PR opened by " + user + "**"
	}
	md := fmt.Sprintf(
		"%s\n[%s#%d](%s): %s",
		header,
		repo, pr.GetNumber(), pr.GetHTMLURL(), escapeMarkdown(pr.GetTitle()),
	)
	if pr.GetUser().GetType() != "Bot" {
		if body := pr.GetBody(); body != "" {
			md += "\n\n" + prepareMarkdown(body, repo)
		}
	}
	return md
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
		if h.editClosingComment(ctx, repo, issue, comment) {
			return
		}
		h.editEntity(ctx, repo, "issue_comment", comment.GetID(), renderIssueComment(issue, comment, repo))
		return
	case "deleted":
		if h.removeClosingComment(ctx, repo, issue, comment) {
			return
		}
		h.deleteEntity(ctx, repo, "issue_comment", comment.GetID())
		return
	case "created":
		// fall through
	default:
		return
	}
	if h.closures.addComment(e) {
		return
	}

	msgID, err := h.sendThreaded(ctx, repo, issue.GetNumber(), renderIssueComment(issue, comment, repo))
	if err != nil {
		log.Printf("failed to send comment notification: %v", err)
		return
	}
	log.Printf("sent comment notification for %s#%d (msg %d)", repo, issue.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, issue.GetNumber(), issue.IsPullRequest(), comment.GetID(), "", false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	if err := h.db.LinkEntity(repo, "issue_comment", comment.GetID(), msgID); err != nil {
		log.Printf("failed to link issue_comment: %v", err)
	}
}

// renderIssueComment produces the Markdown for an issue or PR comment
// notification. Used both at creation and on subsequent edits.
func renderIssueComment(issue *gh.Issue, comment *gh.IssueComment, repo string) string {
	kind := "Issue"
	if issue.IsPullRequest() {
		kind = "PR"
	}
	md := fmt.Sprintf(
		"💬 **Comment on %s by %s**\n[%s#%d](%s): %s",
		kind, escapeMarkdown(comment.GetUser().GetLogin()),
		repo, issue.GetNumber(), comment.GetHTMLURL(), escapeMarkdown(issue.GetTitle()),
	)
	if body := comment.GetBody(); body != "" {
		md += "\n\n" + prepareMarkdown(body, repo)
	}
	return md
}

func (h *Handler) handlePullRequestReview(ctx context.Context, e *gh.PullRequestReviewEvent) {
	action := e.GetAction()
	review := e.GetReview()

	if action == "edited" {
		repo := e.GetRepo().GetFullName()
		h.refreshConsolidatedReview(ctx, repo, e.GetPullRequest(), review.GetID(), true)
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
		if msgID, err := h.db.LookupEntity(repo, "single_review_comment", commentID); err == nil {
			md := renderSingleReviewComment(e.GetPullRequest(), comment, repo)
			if err := h.tg.EditRich(ctx, msgID, md); err != nil {
				log.Printf("failed to edit review comment %d: %v", commentID, err)
				return
			}
			h.tg.React(ctx, msgID, "✍")
			log.Printf("edited single_review_comment msg %d (%s entity %d)", msgID, repo, commentID)
			return
		}
		// Otherwise it may be part of a consolidated review.
		if _, err := h.db.LookupEntity(repo, "consolidated_review_comment", commentID); err == nil {
			h.refreshConsolidatedReview(ctx, repo, e.GetPullRequest(), comment.GetPullRequestReviewID(), true)
		}
		return
	case "deleted":
		if msgID, err := h.db.LookupEntity(repo, "single_review_comment", commentID); err == nil {
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
		if _, err := h.db.LookupEntity(repo, "consolidated_review_comment", commentID); err == nil {
			// Unlink first so the re-render doesn't re-include this comment
			// if GitHub still returns it transiently.
			if err := h.db.UnlinkEntity(repo, "consolidated_review_comment", commentID); err != nil {
				log.Printf("failed to unlink consolidated_review_comment %d: %v", commentID, err)
			}
			h.refreshConsolidatedReview(ctx, repo, e.GetPullRequest(), comment.GetPullRequestReviewID(), false)
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
func (h *Handler) refreshConsolidatedReview(ctx context.Context, repo string, pr *gh.PullRequest, reviewID int64, markEdited bool) {
	msgID, err := h.db.LookupEntity(repo, "consolidated_review", reviewID)
	if err != nil {
		return
	}
	review, comments, err := h.gh.GetReviewWithComments(ctx, repo, pr.GetNumber(), reviewID)
	if err != nil {
		log.Printf("failed to fetch review %d for re-render: %v", reviewID, err)
		return
	}
	md := renderConsolidatedReview(review, comments, pr, repo)
	if err := h.tg.EditRich(ctx, msgID, md); err != nil {
		log.Printf("failed to edit consolidated review %d: %v", reviewID, err)
		return
	}
	if markEdited {
		h.tg.React(ctx, msgID, "✍")
	} else if err := h.tg.ClearReaction(ctx, msgID); err != nil {
		log.Printf("failed to clear edit reaction from consolidated review msg %d: %v", msgID, err)
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

	msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), renderConsolidatedReview(review, comments, pr, repo))
	if err != nil {
		log.Printf("failed to send consolidated review for %s#%d: %v", repo, pr.GetNumber(), err)
		return
	}
	log.Printf("sent consolidated review for %s#%d (%d comments, msg %d)", repo, pr.GetNumber(), len(comments), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, 0, review.GetBody(), false); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	if err := h.db.LinkEntity(repo, "consolidated_review", review.GetID(), msgID); err != nil {
		log.Printf("failed to link consolidated_review: %v", err)
	}
	for _, c := range comments {
		if err := h.db.LinkEntity(repo, "consolidated_review_comment", c.GetID(), msgID); err != nil {
			log.Printf("failed to link consolidated_review_comment %d: %v", c.GetID(), err)
		}
	}
}

func (h *Handler) sendSingleReviewComment(e *gh.PullRequestReviewCommentEvent) {
	ctx := context.Background()
	comment := e.GetComment()
	pr := e.GetPullRequest()
	repo := e.GetRepo().GetFullName()

	msgID, err := h.sendThreaded(ctx, repo, pr.GetNumber(), renderSingleReviewComment(pr, comment, repo))
	if err != nil {
		log.Printf("failed to send review comment notification: %v", err)
		return
	}
	log.Printf("sent review comment notification for %s#%d (msg %d)", repo, pr.GetNumber(), msgID)

	if err := h.db.Save(msgID, repo, pr.GetNumber(), true, comment.GetID(), "", true); err != nil {
		log.Printf("failed to save message mapping: %v", err)
	}
	if err := h.db.LinkEntity(repo, "single_review_comment", comment.GetID(), msgID); err != nil {
		log.Printf("failed to link single_review_comment: %v", err)
	}
}

// renderSingleReviewComment produces the Markdown for a single review-comment
// notification (the non-consolidated path).
func renderSingleReviewComment(pr *gh.PullRequest, comment *gh.PullRequestComment, repo string) string {
	user := escapeMarkdown(comment.GetUser().GetLogin())
	var header string
	if comment.GetInReplyTo() > 0 {
		header = "💬 **Reply by " + user + "**"
	} else {
		header = "💬 **Review comment by " + user + "**"
	}

	md := fmt.Sprintf(
		"%s\n[%s#%d](%s): %s\nOn `%s`:",
		header,
		repo, pr.GetNumber(), comment.GetHTMLURL(), escapeMarkdown(pr.GetTitle()),
		formatCommentLocation(comment),
	)

	if body := comment.GetBody(); body != "" {
		md += "\n\n" + prepareMarkdown(body, repo)
	}
	return md
}

// renderConsolidatedReview produces the Markdown for a consolidated review
// message: the review body + each inline comment, with a "… and N more" tail
// when the rendering would exceed Telegram's text limit. Used both at first
// send and on re-render after edits/deletes.
func renderConsolidatedReview(review *gh.PullRequestReview, comments []*gh.PullRequestComment, pr *gh.PullRequest, repo string) string {
	reviewer := escapeMarkdown(review.GetUser().GetLogin())
	var header string
	switch review.GetState() {
	case "approved":
		header = "✅ **Approved by " + reviewer + "**"
	case "changes_requested":
		header = "🛑 **Changes Requested by " + reviewer + "**"
	case "commented":
		header = "👀 **Reviewed by " + reviewer + "**"
	}

	md := fmt.Sprintf(
		"%s\n[%s#%d](%s): %s",
		header,
		repo, pr.GetNumber(), review.GetHTMLURL(), escapeMarkdown(pr.GetTitle()),
	)

	if body := review.GetBody(); body != "" && review.GetUser().GetType() != "Bot" {
		md += "\n\n" + prepareMarkdown(body, repo)
	}

	if len(comments) > 0 {
		md += fmt.Sprintf("\n\n**%d inline comments**", len(comments))
		// Leave headroom below the rich message limit so the tail below and
		// any truncation marker still fit.
		const maxLen = telegram.MaxRichRunes - 768
		shown := 0
		for _, comment := range comments {
			entry := fmt.Sprintf("\n\n---\n\n📝 `%s`", formatCommentLocation(comment))
			if body := comment.GetBody(); body != "" {
				entry += "\n\n" + prepareMarkdown(body, repo)
			}
			if len([]rune(md))+len([]rune(entry)) > maxLen {
				remaining := len(comments) - shown
				md += fmt.Sprintf("\n\n… and [%d more](%s)", remaining, review.GetHTMLURL())
				break
			}
			md += entry
			shown++
		}
	}

	return md
}

func (h *Handler) editClosingComment(ctx context.Context, repo string, issue *gh.Issue, comment *gh.IssueComment) bool {
	entityType := "issue_closing_comment"
	md := renderIssueClose(issue, repo, comment.GetUser().GetLogin(), comment.GetBody())
	if issue.IsPullRequest() {
		entityType = "pr_closing_comment"
		md = renderPRCloseFromIssue(issue, repo, comment.GetUser().GetLogin(), comment.GetBody())
	}

	msgID, err := h.db.LookupEntity(repo, entityType, comment.GetID())
	if err != nil {
		return false
	}
	if err := h.tg.EditRich(ctx, msgID, md); err != nil {
		log.Printf("failed to edit %s msg %d: %v", entityType, msgID, err)
		return true
	}
	h.tg.React(ctx, msgID, "✍")
	log.Printf("edited %s msg %d (%s entity %d)", entityType, msgID, repo, comment.GetID())
	return true
}

func (h *Handler) removeClosingComment(ctx context.Context, repo string, issue *gh.Issue, comment *gh.IssueComment) bool {
	entityType := "issue_closing_comment"
	md := renderIssueClose(issue, repo, comment.GetUser().GetLogin(), "")
	if issue.IsPullRequest() {
		entityType = "pr_closing_comment"
		md = renderPRCloseFromIssue(issue, repo, comment.GetUser().GetLogin(), "")
	}

	msgID, err := h.db.LookupEntity(repo, entityType, comment.GetID())
	if err != nil {
		return false
	}
	if err := h.tg.EditRich(ctx, msgID, md); err != nil {
		log.Printf("failed to remove %s from msg %d: %v", entityType, msgID, err)
		return true
	}
	if err := h.tg.ClearReaction(ctx, msgID); err != nil {
		log.Printf("failed to clear edit reaction from %s msg %d: %v", entityType, msgID, err)
	}
	if err := h.db.UnlinkEntity(repo, entityType, comment.GetID()); err != nil {
		log.Printf("failed to unlink %s: %v", entityType, err)
	}
	log.Printf("removed %s from msg %d (%s entity %d)", entityType, msgID, repo, comment.GetID())
	return true
}

// editEntity edits the Telegram message linked to a GitHub entity in place.
// No-ops if the entity has never been linked.
func (h *Handler) editEntity(ctx context.Context, repo, entityType string, entityID int64, md string) {
	msgID, err := h.db.LookupEntity(repo, entityType, entityID)
	if err != nil {
		// sql.ErrNoRows is the common case; ignore.
		return
	}
	if err := h.tg.EditRich(ctx, msgID, md); err != nil {
		log.Printf("failed to edit %s msg %d: %v", entityType, msgID, err)
		return
	}
	h.tg.React(ctx, msgID, "✍")
	log.Printf("edited %s msg %d (%s entity %d)", entityType, msgID, repo, entityID)
}

// deleteEntity removes the Telegram message linked to a GitHub entity and
// drops all entity_index entries pointing at that message.
func (h *Handler) deleteEntity(ctx context.Context, repo, entityType string, entityID int64) {
	msgID, err := h.db.LookupEntity(repo, entityType, entityID)
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
