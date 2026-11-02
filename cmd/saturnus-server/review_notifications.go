package main

import (
	"fmt"
	"strings"
	"time"
)

// reviewNotificationsReply displays one complete delivery snapshot per page.
func (s *server) reviewNotificationsReply(id string, page int) botReply {
	req, err := s.reviews.Get(id)
	if err != nil {
		return botReply{text: "Review not found."}
	}
	notifications, err := s.reviews.ListRequesterNotifications(req.ID)
	if err != nil {
		return botReply{text: "Failed to load requester notifications: " + err.Error()}
	}
	if page < 1 || (len(notifications) == 0 && page != 1) || (len(notifications) > 0 && page > len(notifications)) {
		return botReply{text: fmt.Sprintf("Notification page out of range (1-%d).", maxNotificationsPage(len(notifications)))}
	}
	if len(notifications) == 0 {
		return botReply{text: "No requester result notifications for review " + req.ID + "."}
	}
	n := notifications[page-1]
	run := "-"
	for _, r := range s.reviews.ListRuns(req.ID) {
		if r.ID == n.RunID {
			run = fmt.Sprintf("#%d (%s)", r.Seq, r.ID)
			break
		}
	}
	if run == "-" && n.RunID != "" {
		run = n.RunID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Requester notifications for %s — %d/%d\nRun: %s\nRecipient: %s\nTime: %s\nStatus: %s", req.ID, page, len(notifications), run, n.RequesterOpenID, n.CreatedAt.UTC().Format(time.RFC3339), n.Status)
	if n.Error != "" {
		fmt.Fprintf(&b, "\nError: %s", n.Error)
	}
	fmt.Fprintf(&b, "\n\nMessage:\n%s", n.Message)
	if page < len(notifications) {
		fmt.Fprintf(&b, "\n\nNext: /sat notifications %s %d", req.ID, page+1)
	}
	return botReply{text: b.String()}
}

func maxNotificationsPage(count int) int {
	if count == 0 {
		return 1
	}
	return count
}
