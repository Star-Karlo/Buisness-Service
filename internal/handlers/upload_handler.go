package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/storage"
)

// UploadHandler signs S3 URLs. It never sees a file: the browser PUTs straight
// to S3 with the URL this returns, so a large proof-of-delivery photo is never
// buffered by a service instance.
type UploadHandler struct {
	store *storage.Client
}

func NewUploadHandler(store *storage.Client) *UploadHandler {
	return &UploadHandler{store: store}
}

type presignRequest struct {
	Purpose     string `json:"purpose" binding:"required"`
	FileName    string `json:"fileName" binding:"required"`
	ContentType string `json:"contentType" binding:"required"`
	SizeBytes   int64  `json:"sizeBytes" binding:"required"`
}

type presignResponse struct {
	Key       string            `json:"key"`
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresIn int               `json:"expiresIn"`
}

// Presign issues a short-lived PUT.
//
// The company comes from the token, never the request: it is the first segment
// of the key, and letting a caller supply it would let anyone write into
// another tenant's prefix.
func (h *UploadHandler) Presign(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	var req presignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if !h.store.Configured() {
		// 501, not 500: nothing is broken, the feature is simply not
		// provisioned here. The frontend keys its "uploads unavailable"
		// notice off exactly this.
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
			"success": false,
			"message": "File uploads are not configured on this deployment.",
		})
		return
	}

	purpose := storage.Purpose(req.Purpose)
	if err := storage.Validate(purpose, req.ContentType, req.SizeBytes); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	key, err := storage.Key(actor.CompanyID.String(), purpose, req.FileName)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	url, ttl, err := h.store.PresignPut(c.Request.Context(), key, req.ContentType)
	if err != nil {
		response.InternalError(c, "Could not sign the upload.")
		return
	}

	response.OK(c, presignResponse{
		Key:    key,
		URL:    url,
		Method: http.MethodPut,
		// Echoed back because the signature covers Content-Type: a PUT that
		// sends a different one is rejected by S3, and the client must send
		// exactly this.
		Headers:   map[string]string{"Content-Type": req.ContentType},
		ExpiresIn: int(ttl.Seconds()),
	})
}

type downloadRequest struct {
	Key string `json:"key" binding:"required"`
}

// DownloadURL turns a stored key back into a URL a browser can open.
//
// Signed per request rather than stored: a signed URL expires, so persisting
// one on a record would rot, and a permanent public URL would make every
// contract readable by anyone who guessed a key.
func (h *UploadHandler) DownloadURL(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	var req downloadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if !h.store.Configured() {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
			"success": false,
			"message": "File uploads are not configured on this deployment.",
		})
		return
	}

	// Tenant check. Karlo staff are exempt so support can open a client's
	// document, but a company may only read its own — the key alone must
	// never be enough.
	if !actor.PlatformStaff && !storage.OwnedBy(req.Key, actor.CompanyID.String()) {
		response.Forbidden(c, "That file belongs to another company.")
		return
	}

	url, ttl, err := h.store.PresignGet(c.Request.Context(), req.Key)
	if err != nil {
		if errors.Is(err, storage.ErrNotConfigured) {
			response.InternalError(c, "File uploads are not configured.")
			return
		}
		response.InternalError(c, "Could not sign the download.")
		return
	}

	response.OK(c, gin.H{"url": url, "expiresIn": int(ttl.Seconds())})
}
