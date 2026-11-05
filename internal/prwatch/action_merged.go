package prwatch

import (
	"context"
	"log/slog"

	"github.com/boldfield/odonian/internal/notify"
	"github.com/boldfield/odonian/internal/store"
)

type taskTx interface {
	TransitionTask(ctx context.Context, id, toState string, note *string) (store.Task, error)
}

type externalMergeTx interface {
	ConvergeExternallyMergedTask(ctx context.Context, taskID, prURL string) (store.Task, error)
}

func applyMerged(ctx context.Context, tx taskTx, n notify.Notifier, task store.Task) error {
	_, err := tx.TransitionTask(ctx, task.ID, "done", nil)
	if err != nil {
		return err
	}

	notification := notify.Notification{
		Event:    "odonian-merged",
		Title:    "Merged: " + task.Title,
		Priority: 4,
		DedupKey: "odonian-merged:" + task.ID,
	}

	if err := n.Publish(ctx, notification); err != nil {
		slog.Error("failed to publish odonian-merged notification", "task_id", task.ID, "error", err)
	}

	return nil
}

func applyMergedExternally(ctx context.Context, tx externalMergeTx, n notify.Notifier, task store.Task, prURL string) error {
	_, err := tx.ConvergeExternallyMergedTask(ctx, task.ID, prURL)
	if err != nil {
		return err
	}

	notification := notify.Notification{
		Event:    "odonian-merged",
		Title:    "Merged: " + task.Title,
		Priority: 4,
		DedupKey: "odonian-merged:" + task.ID,
	}

	if err := n.Publish(ctx, notification); err != nil {
		slog.Error("failed to publish odonian-merged notification", "task_id", task.ID, "error", err)
	}

	return nil
}

var _ taskTx = store.Store(nil)
var _ externalMergeTx = store.Store(nil)
