package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestNotificationsCoalesceAndDone(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	me, _ := s.CreateOwner(ctx, "zoe", "", "user")
	other, _ := s.CreateOwner(ctx, "pat", "", "user")
	repo, err := s.CreateRepo(ctx, me.ID, "demo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	issue := NewNotification{PersonID: me.ID, Kind: "issue.opened", RepoID: repo.ID, SubjectType: "issue", Number: 3,
		Title: "Crash", URL: "/zoe/demo/issues/3", ActorKind: "token", ActorName: "claude", Reason: "subscribed"}
	id1, err := s.Notify(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNotificationDone(ctx, me.ID, id1); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadNotificationCount(ctx, me.ID); n != 0 {
		t.Fatalf("unread after done = %d", n)
	}

	// New activity on the same subject comes back to the inbox as the same row.
	issue.Kind, issue.Detail = "issue.commented", "looks like a nil map"
	id2, err := s.Notify(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("coalesced id = %d, want %d", id2, id1)
	}
	n, err := s.Notification(ctx, me.ID, id1)
	if err != nil || !n.Unread || n.Done || n.Kind != "issue.commented" || n.Detail != "looks like a nil map" {
		t.Fatalf("after coalesce: %+v err=%v", n, err)
	}

	// Different subjects (another number, a pull, a repo-less backup) are separate rows.
	pull := issue
	pull.SubjectType, pull.Kind = "pull", "pull.opened"
	s.Notify(ctx, pull)
	other3 := issue
	other3.Number = 4
	s.Notify(ctx, other3)
	backup := NewNotification{PersonID: me.ID, Kind: "backup.failed", SubjectType: "backup", Title: "Backup failed", ActorKind: "system"}
	b1, _ := s.Notify(ctx, backup)
	b2, _ := s.Notify(ctx, backup)
	if b1 != b2 {
		t.Errorf("repo-less subject didn't coalesce: %d vs %d", b1, b2)
	}
	// Someone else's notifications are theirs.
	theirs := issue
	theirs.PersonID = other.ID
	tid, _ := s.Notify(ctx, theirs)
	if tid == id1 {
		t.Error("another person's notification coalesced into mine")
	}

	if c, _ := s.UnreadNotificationCount(ctx, me.ID); c != 4 {
		t.Errorf("unread = %d, want 4", c)
	}
	list, _ := s.Notifications(ctx, NotificationQuery{PersonID: me.ID})
	if len(list) != 4 || list[0].ID != b1 {
		t.Fatalf("inbox = %d rows, first %+v", len(list), list)
	}
	if list[1].Repo != "zoe/demo" {
		t.Errorf("repo name = %q", list[1].Repo)
	}
	if got, _ := s.Notifications(ctx, NotificationQuery{PersonID: me.ID, SubjectTypes: []string{"issue"}}); len(got) != 2 {
		t.Errorf("issue filter = %d rows, want 2", len(got))
	}
	if got, _ := s.Notifications(ctx, NotificationQuery{PersonID: me.ID, ReadableRepos: []int64{}}); len(got) != 1 {
		t.Errorf("no readable repos should leave only the backup row, got %d", len(got))
	}
	if got, _ := s.Notifications(ctx, NotificationQuery{PersonID: me.ID, Limit: 2, Offset: 2}); len(got) != 2 {
		t.Errorf("page 2 = %d rows", len(got))
	}

	// Mark read keeps it in the inbox; mark all done honours the filter.
	s.MarkNotificationRead(ctx, me.ID, b1)
	if c, _ := s.UnreadNotificationCount(ctx, me.ID); c != 3 {
		t.Errorf("unread after read = %d, want 3", c)
	}
	moved, err := s.MarkNotificationsDone(ctx, NotificationQuery{PersonID: me.ID, SubjectTypes: []string{"issue"}})
	if err != nil || moved != 2 {
		t.Fatalf("mark issues done moved %d (%v), want 2", moved, err)
	}
	if got, _ := s.Notifications(ctx, NotificationQuery{PersonID: me.ID}); len(got) != 2 {
		t.Errorf("inbox after mark-all = %d, want 2", len(got))
	}
	if got, _ := s.Notifications(ctx, NotificationQuery{PersonID: me.ID, Done: true}); len(got) != 2 {
		t.Errorf("done list = %d, want 2", len(got))
	}
	if c, _ := s.UnreadNotificationCount(ctx, other.ID); c != 1 {
		t.Errorf("other person's unread = %d, want 1 (untouched)", c)
	}
	if err := s.MarkNotificationDone(ctx, other.ID, id1); err != ErrNotFound {
		t.Errorf("marking someone else's notification done: %v", err)
	}

	groups, err := s.NotificationGroups(ctx, NotificationQuery{PersonID: me.ID})
	if err != nil || len(groups) != 2 {
		t.Errorf("groups = %+v (%v)", groups, err)
	}

	// Deleting the repository removes its notifications.
	s.DeleteRepo(ctx, repo.ID)
	if got, _ := s.Notifications(ctx, NotificationQuery{PersonID: me.ID}); len(got) != 1 {
		t.Errorf("after repo delete = %d rows, want 1", len(got))
	}

	first, _ := s.MarkNotificationSourceSeen(ctx, "backup_run", 7)
	again, _ := s.MarkNotificationSourceSeen(ctx, "backup_run", 7)
	if !first || again {
		t.Errorf("source seen: first=%v again=%v", first, again)
	}
}
