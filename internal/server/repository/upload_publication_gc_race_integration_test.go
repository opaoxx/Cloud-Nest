package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadPublicationGCRaceIntegration(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		publicationFirst bool
	}{
		{"publication commits first", true},
		{"GC claim commits first", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备已到期且无引用的 READY 对象，以及同内容的待校验任务。
			ctx, pool, store := prepareMultipartCompletionDatabase(t)
			const userID, taskID, stagedID, oldID, partSize = 7, 19, 23, 29, 16 << 20
			digest := domain.UploadContentDigest{SizeBytes: partSize + 1}
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31")
			execute("UPDATE users SET used_storage=0,reserved_storage=? WHERE id=?", digest.SizeBytes, userID)
			execute(`UPDATE stored_objects SET file_size=?,storage_version_id='fixture-old-version',
				unreferenced_at=UTC_TIMESTAMP(6)-INTERVAL 8 DAY,gc_after=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE id=?`, digest.SizeBytes, oldID)
			execute(`UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='fixture-staged-version',storage_etag='fixture-etag'
				WHERE id=?`, digest.SizeBytes, stagedID)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='MULTIPART',storage_upload_id='fixture-race-session',
				file_size=?,part_size=?,part_count=2,reserved_bytes=?,expected_sha256=?,lease_owner=NULL,lease_until=NULL,lease_version=0,
				next_attempt_at=NULL,published_object_id=NULL,result_entry_id=NULL,resolved_name=NULL,completed_at=NULL,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=?`, digest.SizeBytes, partSize, digest.SizeBytes, digest.SHA256[:], taskID)
			candidates, err := store.ListStoredObjectGCCandidates(ctx, 2)
			if err != nil || len(candidates) != 1 || candidates[0].ObjectID != oldID {
				t.Fatal("没有扫描到旧对象回收候选", candidates, err)
			}
			gcCandidate := candidates[0]
			tasks, err := store.ListMultipartUploadVerificationCandidates(ctx, 2)
			if err != nil || len(tasks) != 1 || tasks[0].TaskID != taskID {
				t.Fatal("没有扫描到新上传的校验候选", tasks, err)
			}
			task, err := store.ClaimMultipartUploadVerificationLease(ctx, tasks[0], "fixture-publication-race")
			if err != nil || task == nil {
				t.Fatal("未领取本次发布的校验租约", err)
			}
			readObject := func(objectID uint64) *domain.StoredObject {
				t.Helper()
				object, err := store.FindUserStoredObject(ctx, userID, objectID)
				if err != nil || object == nil {
					t.Fatal("读取合成对象失败", objectID, err)
				}
				return object
			}
			staged := readObject(stagedID)

			// 2. 在指定的发布边界暂停，确保提交顺序能由测试显式控制。
			workContext, stopWork := context.WithTimeout(ctx, 15*time.Second)
			var workers sync.WaitGroup
			defer func() {
				stopWork()
				workers.Wait()
			}()
			atBoundary := make(chan struct{})
			release := make(chan struct{})
			pausePublication := func(ctx context.Context) error {
				close(atBoundary)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, object *domain.StoredObject, digest domain.UploadContentDigest,
				action func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
				if !scenario.publicationFirst {
					if err := pausePublication(ctx); err != nil {
						return err
					}
				}
				return store.WithUploadVerificationTransaction(ctx, task, object, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
					if err := action(txContext, txStore, current, staged); err != nil {
						return err
					}
					if scenario.publicationFirst {
						return pausePublication(txContext)
					}
					return nil
				})
			})
			type publicationResult struct {
				task *domain.UploadTask
				err  error
			}
			type gcResult struct {
				object *domain.StoredObject
				err    error
			}
			published := make(chan publicationResult, 1)
			workers.Go(func() {
				result, err := publisher.Publish(workContext, task, staged, digest)
				published <- publicationResult{task: result, err: err}
			})
			select {
			case <-atBoundary:
			case early := <-published:
				t.Fatal("发布未到达约定边界", early.err)
			case <-workContext.Done():
				t.Fatal("等待发布边界超时", workContext.Err())
			}

			var claimed *domain.StoredObject
			if scenario.publicationFirst {
				// 回收入口已启动，发布事务仍持有账号锁。允许它先提交新引用。
				gcStarted := make(chan struct{})
				gcFinished := make(chan gcResult, 1)
				workers.Go(func() {
					close(gcStarted)
					object, err := store.ClaimStoredObjectGC(workContext, gcCandidate)
					gcFinished <- gcResult{object: object, err: err}
				})
				<-gcStarted
				close(release)
				result := <-gcFinished
				if result.object != nil || !errors.Is(result.err, domain.ErrStoredObjectGCCandidateStale) {
					t.Fatal("新引用提交后，旧回收候选应失效", result.err)
				}
			} else {
				// 发布尚未开启写回事务。先让真实回收入口提交删除意图。
				claimed, err = store.ClaimStoredObjectGC(workContext, gcCandidate)
				if err != nil || claimed == nil || claimed.Status != domain.StoredObjectStatusDeleting {
					t.Fatal("回收未先提交 DELETING 状态", err)
				}
				close(release)
			}

			// 3. 验证提交后的新引用只指向 READY 对象，且仅结算一次空间。
			result := <-published
			if result.err != nil || result.task == nil || result.task.Status != domain.UploadStatusCompleted ||
				result.task.PublishedObjectID == nil || result.task.ResultEntryID == nil || result.task.ReservedBytes != 0 {
				t.Fatal("上传未完成发布与配额结算", result.err)
			}
			wantID := uint64(stagedID)
			old := readObject(oldID)
			if scenario.publicationFirst {
				wantID = oldID
				if old.Status != domain.StoredObjectStatusReady || old.UnreferencedAt != nil || old.GCAfter != nil ||
					readObject(stagedID).Status != domain.StoredObjectStatusOrphaned {
					t.Fatal("旧对象未受新引用保护，或冗余对象未进入回收期")
				}
			} else if old.Status != domain.StoredObjectStatusDeleting || !sameStoredObjectLocation(old, claimed) {
				t.Fatal("发布更改了已领取回收的旧对象")
			}
			selected := readObject(wantID)
			content, err := store.FindUserFileEntryContent(ctx, userID, *result.task.ResultEntryID)
			if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != wantID || *result.task.PublishedObjectID != wantID ||
				selected.Status != domain.StoredObjectStatusReady || selected.VerifiedSHA256 == nil || *selected.VerifiedSHA256 != digest.SHA256 {
				t.Fatal("新文件没有引用预期的已验证对象", err)
			}
			var used, reserved uint64
			var activeFiles int
			if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
				(SELECT COUNT(*) FROM file_entries WHERE user_id=u.id AND entry_type='FILE' AND status='ACTIVE') FROM users u WHERE u.id=?`, userID).
				Scan(&used, &reserved, &activeFiles); err != nil || used != digest.SizeBytes || reserved != 0 || activeFiles != 1 {
				t.Fatal("竞争后的账号账目或活动文件数不符", used, reserved, activeFiles, err)
			}
			t.Logf("提交顺序 %s，旧对象 %s，新文件引用 READY 对象 %d，已用 %d B", scenario.name, old.Status, wantID, used)
		})
	}
}
