package service

import (
	"context"
	"path/filepath"
	"sync"
)

// ReviewDirName is the detached worktree under .slai/worktrees where review_diff puts the commit
// under review, so the principal can run its tests on exactly that commit: by review time the
// engineer's own worktree is usually on its next task. slai owns it; no agent is assigned to it.
const ReviewDirName = "_review"

var reviewCheckoutMu sync.Mutex // one reviewer, one checkout: calls take turns

// ReviewDir is the path of the review checkout.
func (s *Service) ReviewDir() string {
	return filepath.Join(s.Dir, ".slai", "worktrees", ReviewDirName)
}

// reviewCheckout detaches the review worktree at sha (recreating it clean) and returns its path.
func (s *Service) reviewCheckout(ctx context.Context, sha string) (string, error) {
	reviewCheckoutMu.Lock()
	defer reviewCheckoutMu.Unlock()
	dir := s.ReviewDir()
	if err := s.Git.AddDetached(ctx, dir, sha); err != nil {
		return "", err
	}
	return dir, nil
}
