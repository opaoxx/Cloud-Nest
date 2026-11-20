package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompletePurgeLeafBatch 在一个根项事务中结算一批叶子。
// 返回已提交的项数，失败时返回零和错误。
func (store *Store) CompletePurgeLeafBatch(ctx context.Context, root domain.PurgeRootCandidate, limit int) (int, error) {
	// 1. 锁定并复核清理根项，开启本批事务。
	var completed int
	err := store.WithPurgeRootTransaction(ctx, root, func(txContext context.Context, txStore *Store) error {
		// 2. 在事务内限量读取当前可清理的叶子。
		candidates, err := txStore.ListPurgeLeafCandidates(txContext, limit)
		if err != nil {
			return err
		}

		// 3. 逐项调用单项结算，遇到错误就结束本批。
		for _, candidate := range candidates {
			if err := txStore.CompletePurgeLeaf(txContext, candidate); err != nil {
				return fmt.Errorf("结算永久清理叶子 %d 失败: %w", candidate.EntryID, err)
			}
		}
		completed = len(candidates)
		return nil
	})

	// 4. 确认事务提交后，再返回已完成项数。
	if err != nil {
		return 0, err
	}
	return completed, nil
}
