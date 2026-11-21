package handler

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// UploadCompletionByMode 根据当前账号所属任务的上传模式处理完成请求。
// 路由须先挂载认证中间件。completeMultipart 只在需要新回执时调用读取函数。
func UploadCompletionByMode(
	getTask func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
	completeMultipart func(context.Context, domain.Actor, uint64, func() ([]domain.UploadPartCompletionClaim, error)) (*domain.UploadTask, error),
) gin.HandlerFunc {
	if getTask == nil || completeMultipart == nil {
		panic("按上传模式处理完成请求需要任务查询和分片完成服务")
	}
	return func(requestContext *gin.Context) {
		// 1. 复用认证和路径校验，读取当前账号的任务。
		ctx, actor, taskID, ready := prepareUploadTaskAction(requestContext)
		if !ready {
			return
		}
		task, err := getTask(ctx, actor, taskID)
		if ctx.Err() != nil || err != nil {
			writeMultipartUploadCompletionFailure(requestContext, err)
			return
		}
		if task == nil || task.ID != taskID || task.UserID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}

		// 2. 按上传模式确认完成，只在分片服务需要新回执时解析正文。
		switch task.UploadMode {
		case "SINGLE":
			if _, err := domain.ResolveSingleUploadCompletionStatus(task); err != nil {
				writeUploadCompletionFailure(requestContext, err)
				return
			}
		case "MULTIPART":
			task, err = completeMultipart(ctx, actor, taskID, func() ([]domain.UploadPartCompletionClaim, error) {
				return readMultipartUploadCompletionClaims(requestContext)
			})
			if requestContext.Writer.Written() || requestContext.IsAborted() {
				requestContext.Abort()
				return
			}
			if ctx.Err() != nil || err != nil {
				writeMultipartUploadCompletionFailure(requestContext, err)
				return
			}
			if task == nil || task.ID != taskID || task.UserID != actor.UserID || task.UploadMode != "MULTIPART" {
				InternalError(requestContext, nil)
				return
			}
		default:
			InternalError(requestContext, nil)
			return
		}

		// 3. 交付已核对归属及模式的当前进度或发布结果。
		UploadCompletionSuccess(requestContext, task)
	}
}

func readMultipartUploadCompletionClaims(requestContext *gin.Context) ([]domain.UploadPartCompletionClaim, error) {
	// 1. 缺少正文按回执字段错误处理。其他输入复用现有 JSON 绑定。
	var request dto.CompleteMultipartUploadRequest
	bound, empty := binding.ReadJSONIfPresent(requestContext, &request)
	if empty {
		return nil, &domain.UploadPartDeclarationError{Field: "parts", Message: "完成请求须提交分片回执"}
	}
	if !bound {
		// 绑定器已交付错误，返回失败以终止应用流程。
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 按回执契约校验，交付稳定的公开字段错误或完整声明。
	claims, fieldError := dto.ParseCompleteMultipartUploadRequest(request)
	if fieldError != nil {
		return nil, &domain.UploadPartDeclarationError{Field: fieldError.Field, Message: fieldError.Message}
	}
	return claims, nil
}
