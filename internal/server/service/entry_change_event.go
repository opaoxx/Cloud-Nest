package service

import (
	"encoding/json"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// newEntryChangeEvent 从调用方已核对的普通目录项编码当前名称与位置。
func newEntryChangeEvent(eventType string, entry *domain.FileEntry) (domain.ChangeEventInput, error) {
	payload, err := json.Marshal(struct {
		EntryID  string `json:"entry_id"`
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
		Revision string `json:"revision"`
	}{
		EntryID:  strconv.FormatUint(entry.ID, 10),
		ParentID: strconv.FormatUint(*entry.ParentID, 10),
		Name:     entry.Name,
		Revision: strconv.FormatUint(entry.Revision, 10),
	})
	if err != nil {
		return domain.ChangeEventInput{}, err
	}
	return domain.ChangeEventInput{
		EntryID: entry.ID, Revision: entry.Revision, EventType: eventType, Payload: payload,
	}, nil
}
