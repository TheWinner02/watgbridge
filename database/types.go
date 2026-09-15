package database

import (
	"database/sql"
	"time"

	"watgbridge/state"
)

type MsgIdPair struct {
	// WhatsApp
	ID            string `gorm:"primaryKey;"` // Message ID
	ParticipantId string // Sender JID
	WaChatId      string // Chat JID

	// Telegram
	TgChatId   int64
	TgThreadId int64
	TgMsgId    int64

	MarkRead sql.NullBool
	AutoReacted bool
}

type ChatThreadPair struct {
	ID         string `gorm:"primaryKey;"` // WhatsApp Chat ID
	TgChatId   int64  // Telegram Chat ID
	TgThreadId int64  // Telegram Thread ID (Topics)
}

type ContactName struct {
	ID           string `gorm:"primaryKey;"` // WhatsApp Contact JID
	FirstName    string
	FullName     string
	PushName     string
	BusinessName string
	Server       string
}

type ChatEphemeralSettings struct {
	ID             string `gorm:"primaryKey;"` // WhatsApp Chat ID
	IsEphemeral    bool
	EphemeralTimer uint32
}

type MessageReceipt struct {
	WaMsgId       string    `gorm:"primaryKey;index:idx_receipt_msg_chat_participant"`
	WaChatId      string    `gorm:"index:idx_receipt_msg_chat_participant"`
	ParticipantId string    `gorm:"primaryKey;index:idx_receipt_msg_chat_participant"`
	ReceiptType   string    `gorm:"primaryKey"`
	ReceiptTime   time.Time
}

type PollPair struct {
	PollID      string `gorm:"primaryKey;"` // Telegram Poll ID
	WaMsgID     string `gorm:"index"`       // WhatsApp Message ID
	WaChatID    string                      // WhatsApp Chat JID
	WaSenderID  string                      // WhatsApp Sender JID (for encryption)
	TgChatID    int64
	TgThreadID  int64
	TgMsgID     int64
	OptionNames string // JSON array of string options
}

func AutoMigrate() error {
	db := state.State.Database
	return db.AutoMigrate(
		&MsgIdPair{},
		&ChatThreadPair{},
		&ContactName{},
		&ChatEphemeralSettings{},
		&MessageReceipt{},
		&PollPair{},
	)
}
