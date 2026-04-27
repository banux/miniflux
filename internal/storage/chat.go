// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"miniflux.app/v2/internal/model"
)

// CreateChatConversation inserts an empty conversation owned by the user and
// returns the populated row. Title is intentionally optional — the agent
// fills it from the first user message later.
func (s *Storage) CreateChatConversation(userID int64, title string) (*model.ChatConversation, error) {
	conv := &model.ChatConversation{}
	err := s.db.QueryRow(
		`INSERT INTO chat_conversations (user_id, title)
		 VALUES ($1, $2)
		 RETURNING id, user_id, title, created_at, updated_at`,
		userID, title,
	).Scan(&conv.ID, &conv.UserID, &conv.Title, &conv.CreatedAt, &conv.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to create chat conversation: %v`, err)
	}
	return conv, nil
}

// ChatConversations lists conversations for the user, most recently active
// first. Messages are NOT loaded — use ChatConversationByID for that.
func (s *Storage) ChatConversations(userID int64) ([]*model.ChatConversation, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, title, created_at, updated_at
		 FROM chat_conversations
		 WHERE user_id = $1
		 ORDER BY updated_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to list chat conversations: %v`, err)
	}
	defer rows.Close()

	out := make([]*model.ChatConversation, 0)
	for rows.Next() {
		c := &model.ChatConversation{}
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf(`store: unable to scan chat conversation: %v`, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// ChatConversationByID returns the conversation if it belongs to the user,
// with all messages eager-loaded in chronological order.
func (s *Storage) ChatConversationByID(userID, id int64) (*model.ChatConversation, error) {
	conv := &model.ChatConversation{}
	err := s.db.QueryRow(
		`SELECT id, user_id, title, created_at, updated_at
		 FROM chat_conversations
		 WHERE id = $1 AND user_id = $2`,
		id, userID,
	).Scan(&conv.ID, &conv.UserID, &conv.Title, &conv.CreatedAt, &conv.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch chat conversation #%d: %v`, id, err)
	}

	conv.Messages, err = s.chatMessagesForConversation(conv.ID)
	if err != nil {
		return nil, err
	}
	return conv, nil
}

func (s *Storage) chatMessagesForConversation(convID int64) ([]*model.ChatMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, conversation_id, role, content, tool_calls, tool_name, created_at
		 FROM chat_messages
		 WHERE conversation_id = $1
		 ORDER BY id ASC`,
		convID,
	)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch chat messages: %v`, err)
	}
	defer rows.Close()

	out := make([]*model.ChatMessage, 0)
	for rows.Next() {
		m := &model.ChatMessage{}
		var toolCalls sql.NullString
		var toolName sql.NullString
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &toolCalls, &toolName, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf(`store: unable to scan chat message: %v`, err)
		}
		if toolCalls.Valid && toolCalls.String != "" {
			if err := json.Unmarshal([]byte(toolCalls.String), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf(`store: unable to decode tool_calls for message #%d: %v`, m.ID, err)
			}
		}
		if toolName.Valid {
			m.ToolName = toolName.String
		}
		out = append(out, m)
	}
	return out, nil
}

// AppendChatMessage stores one message at the end of the conversation and
// bumps the conversation's updated_at. Caller must have already verified the
// conversation belongs to the user.
func (s *Storage) AppendChatMessage(m *model.ChatMessage) error {
	var toolCallsJSON sql.NullString
	if len(m.ToolCalls) > 0 {
		body, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return fmt.Errorf(`store: unable to encode tool_calls: %v`, err)
		}
		toolCallsJSON.Valid = true
		toolCallsJSON.String = string(body)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf(`store: unable to begin chat append tx: %v`, err)
	}
	defer tx.Rollback()

	if err := tx.QueryRow(
		`INSERT INTO chat_messages (conversation_id, role, content, tool_calls, tool_name)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		 RETURNING id, created_at`,
		m.ConversationID, m.Role, m.Content, toolCallsJSON, m.ToolName,
	).Scan(&m.ID, &m.CreatedAt); err != nil {
		return fmt.Errorf(`store: unable to append chat message: %v`, err)
	}
	if _, err := tx.Exec(`UPDATE chat_conversations SET updated_at = now() WHERE id = $1`, m.ConversationID); err != nil {
		return fmt.Errorf(`store: unable to bump conversation timestamp: %v`, err)
	}
	return tx.Commit()
}

// SetChatConversationTitle updates the title only when it is empty so the
// auto-generated title never overrides one a (future) user-rename action set.
func (s *Storage) SetChatConversationTitle(userID, id int64, title string) error {
	if _, err := s.db.Exec(
		`UPDATE chat_conversations
		 SET title = $1
		 WHERE id = $2 AND user_id = $3 AND title = ''`,
		title, id, userID,
	); err != nil {
		return fmt.Errorf(`store: unable to set chat conversation title: %v`, err)
	}
	return nil
}

// DeleteChatConversation removes a conversation (and its messages via cascade)
// only if it belongs to the user.
func (s *Storage) DeleteChatConversation(userID, id int64) error {
	if _, err := s.db.Exec(
		`DELETE FROM chat_conversations WHERE id = $1 AND user_id = $2`,
		id, userID,
	); err != nil {
		return fmt.Errorf(`store: unable to delete chat conversation #%d: %v`, id, err)
	}
	return nil
}
