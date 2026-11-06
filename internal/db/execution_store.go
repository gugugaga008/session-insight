package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/bbsteel/session-insight/internal/execution"
)

func (db *DB) LoadExecution(key execution.Key) (execution.State, bool, error) {
	var raw string
	err := db.conn.QueryRow(`SELECT state_json FROM execution_boards WHERE agent_type=? AND session_id=?`, key.Agent, key.Session).Scan(&raw)
	if err == sql.ErrNoRows {
		return execution.State{}, false, nil
	}
	if err != nil {
		return execution.State{}, false, err
	}
	var state execution.State
	if err = json.Unmarshal([]byte(raw), &state); err != nil {
		return state, false, fmt.Errorf("decode execution state: %w", err)
	}
	return state, true, nil
}
func (db *DB) SaveExecution(key execution.Key, state execution.State) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = db.conn.Exec(`INSERT INTO execution_boards(agent_type,session_id,enabled,state_json) VALUES(?,?,?,?)
	ON CONFLICT(agent_type,session_id) DO UPDATE SET enabled=excluded.enabled,state_json=excluded.state_json`, key.Agent, key.Session, state.Board.Enabled, string(raw))
	return err
}
func (db *DB) EnabledExecutions() ([]execution.Record, error) {
	rows, err := db.conn.Query(`SELECT agent_type,session_id,state_json FROM execution_boards WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []execution.Record{}
	for rows.Next() {
		var r execution.Record
		var raw string
		if err = rows.Scan(&r.Key.Agent, &r.Key.Session, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &r.State); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
