package store

import (
	"database/sql"
	"fmt"
)

// AgentProjectIDs are the projects an assistant serves when it does not
// serve all of them, sorted.
func (s *Store) AgentProjectIDs(agentID string) (_ []string, err error) {
	action := fmt.Sprintf("store: read the projects of assistant %s", agentID)
	rows, err := s.reader().Query(`SELECT project_id FROM agent_projects WHERE agent_id = ? ORDER BY project_id`, agentID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return ids, nil
}

// ServedProjects are the projects an assistant serves, by name: every
// unarchived project when it serves all of them, otherwise the ones it
// names, archived or not.
func (s *Store) ServedProjects(agentID string) (_ []Project, err error) {
	action := fmt.Sprintf("store: read the projects assistant %s serves", agentID)
	rows, err := s.reader().Query(`SELECT `+projectColumns+` FROM projects
		WHERE EXISTS (SELECT 1 FROM agents WHERE agents.id = ?1 AND agents.all_projects = 1 AND projects.archived = 0)
		   OR EXISTS (SELECT 1 FROM agent_projects WHERE agent_projects.agent_id = ?1 AND agent_projects.project_id = projects.id)
		ORDER BY name COLLATE NOCASE, id`, agentID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	out := []Project{}
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
}

// SetAgentProjects sets the projects an assistant serves: every one when
// all is set, otherwise projectIDs. Found is false when there is no such
// assistant.
func (s *Store) SetAgentProjects(agentID string, all bool, projectIDs []string) (agent StandingAgent, found bool, err error) {
	action := fmt.Sprintf("store: set the projects of assistant %s", agentID)
	tx, err := s.db.Begin()
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	result, err := tx.Exec(`UPDATE agents SET all_projects = ? WHERE id = ?`, all, agentID)
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: %w", action, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: %w", action, err)
	}
	if n == 0 {
		return StandingAgent{}, false, nil
	}
	if err := setAgentProjectsTx(tx, agentID, all, projectIDs); err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: %w", action, err)
	}
	agent, err = scanStandingAgent(tx.QueryRow(`SELECT `+standingAgentColumns+` FROM agents WHERE id = ?`, agentID))
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: read it back: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return agent, true, nil
}

// setAgentProjectsTx replaces an assistant's project rows: none when it
// serves all projects, otherwise one per id. An id that names no project
// fails the foreign key.
func setAgentProjectsTx(tx *sql.Tx, agentID string, all bool, projectIDs []string) error {
	if _, err := tx.Exec(`DELETE FROM agent_projects WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	if all {
		return nil
	}
	for _, id := range projectIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_projects (agent_id, project_id) VALUES (?, ?)`, agentID, id); err != nil {
			return fmt.Errorf("serve project %s: %w", id, err)
		}
	}
	return nil
}
