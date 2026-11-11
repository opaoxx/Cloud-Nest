// Package storage 封装对象存储访问和连接生命周期。
package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/opaoxx/cloud-nest/internal/server/config"
)

// Client 保存服务端访问 MinIO 的客户端和目标桶。
type Client struct {
	// sdkClient 承载签名请求，SDK 类型不向 HTTP 层暴露。
	sdkClient *minio.Client
	// transport 由本客户端独占，供失败和退出时清理空闲连接。
	transport *http.Transport
	// bucket 是已经过配置校验的目标桶名。
	bucket string
}

// NewClient 使用已校验的配置创建客户端，并在最多 5 秒内检查已有桶可访问。
// 失败返回 nil 和错误，成功返回的客户端由调用方在使用结束后关闭。
func NewClient(ctx context.Context, storageConfig config.StorageConfig) (*Client, error) {
	// 1. 根据内部访问地址和凭证创建 SDK 客户端。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if storageConfig.EndpointURL == nil {
		return nil, fmt.Errorf("缺少已解析的 MinIO 内部访问地址")
	}
	secure := storageConfig.EndpointURL.Scheme == "https"
	transport, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO HTTP 连接管理对象失败")
	}
	transport.ResponseHeaderTimeout = 5 * time.Second
	transport.TLSHandshakeTimeout = 5 * time.Second
	sdkClient, err := minio.New(storageConfig.EndpointURL.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(storageConfig.AccessKey, storageConfig.SecretKey, ""),
		Secure:       secure,
		Transport:    transport,
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1,
	})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("创建 MinIO 客户端失败，请检查内部访问地址")
	}

	// 2. 保存桶名与连接管理对象，明确清理责任。
	client := &Client{sdkClient: sdkClient, transport: transport, bucket: storageConfig.Bucket}

	// 3. 只读检查已有桶，失败时释放连接并返回错误。
	if err := client.CheckBucket(ctx); err != nil {
		client.Close()
		return nil, err
	}

	// 4. 返回通过检查的存储客户端。
	return client, nil
}

// CheckBucket 在最多 5 秒内只读检查目标桶存在且可访问，供启动或就绪检查复用。
// 不创建桶或对象；错误不回显原始响应，取消与超时可通过 errors.Is 判断。
func (client *Client) CheckBucket(ctx context.Context) error {
	// 1. 为本次检查设置期限，保留调用方的更短期限与取消通知。
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// 2. SDK 执行签名的只读请求，发生错误时只返回可公开的本地诊断。
	exists, err := client.sdkClient.BucketExists(checkContext, client.bucket)
	if err != nil {
		if contextError := checkContext.Err(); contextError != nil {
			return fmt.Errorf("MinIO 桶访问检查已取消或超时: %w", contextError)
		}
		// 传输层可先于上下文计时器返回超时，统一保留可判断的超时原因。
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return fmt.Errorf("MinIO 桶访问检查已超时: %w", context.DeadlineExceeded)
		}
		errorResponse := minio.ToErrorResponse(err)
		if errorResponse.StatusCode != 0 {
			return fmt.Errorf("MinIO 桶访问检查失败（HTTP %d），请核对桶、凭证和权限", errorResponse.StatusCode)
		}
		return fmt.Errorf("MinIO 桶访问检查失败，请检查内部地址、网络或 TLS 证书")
	}

	// 3. 桶不存在时停止启动，桶的初始化由独立的环境准备流程负责。
	if !exists {
		return fmt.Errorf("MinIO 存储桶不存在，请先完成桶初始化")
	}
	return nil
}

// Close 释放本客户端独占的空闲 HTTP 连接，可重复调用。
// 它不会中断在途请求；调用方应先结束 HTTP 服务和其他存储使用者。
func (client *Client) Close() {
	// 1. 在业务使用结束后释放可复用的空闲网络连接。
	client.transport.CloseIdleConnections()
}
