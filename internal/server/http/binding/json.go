// Package binding 负责 HTTP 输入校验、正文绑定和统一输入错误。
package binding

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// MaxJSONBodyBytes 是普通 JSON 接口正文的字节上限，不用于文件上传流。
const MaxJSONBodyBytes int64 = 1 << 20

// ReadJSON 将一个 JSON 对象读入 destination；调用方必须传入非 nil 的请求结构体指针。
// 成功返回 true；输入错误返回统一响应及 false，调用方必须立即 return，不能使用部分解析的数据。
// 字段必填、长度与业务规则由后续校验负责；调用方式错误交给已有 Recovery 返回 500。
func ReadJSON(requestContext *gin.Context, destination any) bool {
	bound, _ := readJSON(requestContext, destination, false)
	return bound
}

// ReadJSONIfPresent 与 ReadJSON 使用相同的 JSON 规则。
// 正文缺失时返回 false, true，留给调用方交付字段错误。其他失败仍由绑定器写出。
func ReadJSONIfPresent(requestContext *gin.Context, destination any) (bound, empty bool) {
	return readJSON(requestContext, destination, true)
}

func readJSON(requestContext *gin.Context, destination any, reportEmptyBody bool) (bound, empty bool) {
	// 1. 校验 Content-Type 和正文编码，只接收未压缩的 UTF-8 application/json。
	request := requestContext.Request
	if reportEmptyBody && (request.Body == nil || request.Body == http.NoBody) {
		return false, true
	}
	contentTypes := request.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return abortRequest(requestContext, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "请求必须使用 application/json，字符集为 UTF-8，且不压缩正文"), false
	}
	mediaType, parameters, parseError := mime.ParseMediaType(contentTypes[0])
	charset, charsetDeclared := parameters["charset"]
	hasSupportedCharset := !charsetDeclared || strings.EqualFold(charset, "utf-8")
	encodings := request.Header.Values("Content-Encoding")
	hasSupportedEncoding := len(encodings) == 0 || (len(encodings) == 1 && strings.EqualFold(strings.TrimSpace(encodings[0]), "identity"))
	if parseError != nil || mediaType != "application/json" || !hasSupportedCharset || !hasSupportedEncoding {
		return abortRequest(requestContext, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "请求必须使用 application/json，字符集为 UTF-8，且不压缩正文"), false
	}

	// 2. 用 MaxBytesReader 限制实际读取量，识别正文超限和读取失败。
	if request.Body == nil {
		request.Body = http.NoBody
	}
	request.Body = http.MaxBytesReader(requestContext.Writer, request.Body, MaxJSONBodyBytes)
	defer request.Body.Close()
	body, readError := io.ReadAll(request.Body)
	if readError != nil {
		var sizeError *http.MaxBytesError
		if errors.As(readError, &sizeError) {
			return abortRequest(requestContext, http.StatusRequestEntityTooLarge, "REQUEST_BODY_TOO_LARGE", "JSON 请求正文不能超过 1 MiB"), false
		}
		return abortRequest(requestContext, http.StatusBadRequest, "INVALID_ARGUMENT", "读取请求正文失败"), false
	}
	if reportEmptyBody && len(body) == 0 {
		return false, true
	}

	// 3. 将单个 JSON 对象解析到请求结构体，拒绝未知字段和类型不匹配。
	if !utf8.Valid(body) {
		return abortRequest(requestContext, http.StatusBadRequest, "INVALID_ARGUMENT", "请求正文必须使用有效的 UTF-8 编码"), false
	}
	trimmedBody := bytes.Trim(body, " \t\r\n")
	if len(trimmedBody) == 0 || trimmedBody[0] != '{' {
		return abortRequest(requestContext, http.StatusBadRequest, "INVALID_ARGUMENT", "请求正文必须是单个 JSON 对象"), false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decodeError := decoder.Decode(destination); decodeError != nil {
		var destinationError *json.InvalidUnmarshalError
		if errors.As(decodeError, &destinationError) {
			panic("ReadJSON 必须传入非 nil 的请求结构体指针")
		}
		return abortRequest(requestContext, http.StatusBadRequest, "INVALID_ARGUMENT", "JSON 格式、字段或字段类型不正确"), false
	}

	// 4. 确认正文没有第二段 JSON；成功后交给调用方继续业务校验。
	var trailingValue any
	if decoder.Decode(&trailingValue) != io.EOF {
		return abortRequest(requestContext, http.StatusBadRequest, "INVALID_ARGUMENT", "请求正文只能包含一个 JSON 对象"), false
	}
	return true, false
}
