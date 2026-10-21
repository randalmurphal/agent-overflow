package store

// Migration v155 makes a standing agent a record of its own rather than a
// mark on a thread (docs/specs/standing-agents.md, "Standing agent").
//
// `agents` is the assistant: its own id, its current thread, its home, its
// tick setting and the assistant that created it. The unique `thread_id`
// keeps one current thread per assistant, and it cascades from that thread,
// so deleting the current thread deletes the assistant. `agent_past_threads`
// lists the threads an assistant had before, with when each was superseded;
// it cascades from the thread and from the assistant, so deleting a past
// thread leaves the assistant alone and deleting the assistant leaves its
// past threads as ordinary threads. `moved_draft` and
// `moved_draft_attachments` are the text and attachment ids of the composer
// draft the handoff moved from the past thread to the next one, so a send
// of that draft from the past thread can be told where it went; empty when
// none moved. `thread_requests.made_in_thread_id` is
// the thread that made a request a handoff moved to its assistant's next
// thread, empty when the caller made it. `remote_watches` gains its twin,
// `launched_in_thread_id`: the thread that launched the job, which the
// destination's receipt names, while `thread_id` is the thread that owns
// the watch and receives the completion; every existing watch was launched
// by its own thread.
//
// An assistant serves projects (docs/specs/standing-agents.md, ruling 37):
// every project when `all_projects` is set, otherwise the projects
// `agent_projects` names, each row going with its assistant or its project.
// `agent_helpers` names the threads an assistant spawned without a project,
// helpers to its own work that live under it; a row goes with its thread or
// its assistant.
//
// Every standing_agents row becomes an assistant whose id is its thread's
// id, so the home `<config>/agents/<thread-id>` the promotion made is the
// record's own `<config>/agents/<agent-id>`, serving its thread's project.
// Its thread moves into the home, outside every project, with no group,
// worktree, branch or recorded git origin, as a new assistant's is made.
const agentRecordsMigrationVersion = 155

const agentRecordsV155SQL = `
CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    thread_id        TEXT NOT NULL UNIQUE REFERENCES threads(id) ON DELETE CASCADE,
    home             TEXT NOT NULL,
    notify_on_tick   INTEGER NOT NULL DEFAULT 0 CHECK(notify_on_tick IN (0,1)),
    creator_agent_id TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    all_projects     INTEGER NOT NULL DEFAULT 0 CHECK(all_projects IN (0,1))
);
CREATE TABLE agent_projects (
    agent_id   TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (agent_id, project_id)
) WITHOUT ROWID;
CREATE INDEX idx_agent_projects_project ON agent_projects(project_id);
CREATE TABLE agent_helpers (
    thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    agent_id  TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE
);
CREATE INDEX idx_agent_helpers_agent ON agent_helpers(agent_id);
CREATE TABLE agent_past_threads (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    superseded_at    INTEGER NOT NULL,
    moved_draft      TEXT NOT NULL DEFAULT '',
    moved_draft_attachments TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_agent_past_threads_agent ON agent_past_threads(agent_id, superseded_at DESC);
INSERT INTO agents (id, thread_id, home, notify_on_tick, creator_agent_id, created_at)
    SELECT thread_id, thread_id, home, notify_on_tick, '', promoted_at FROM standing_agents;
INSERT INTO agent_projects (agent_id, project_id)
    SELECT agents.id, threads.project_id FROM agents JOIN threads ON threads.id = agents.thread_id
     WHERE threads.project_id IS NOT NULL;
UPDATE threads SET project_id = NULL, workspace_path = (SELECT home FROM agents WHERE agents.thread_id = threads.id),
       worktree_path = NULL, branch = NULL, group_id = NULL,
       created_branch = '', created_remote_url = '', created_head_commit = ''
 WHERE id IN (SELECT thread_id FROM agents);
DROP TABLE standing_agents;
ALTER TABLE thread_requests ADD COLUMN made_in_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE remote_watches ADD COLUMN launched_in_thread_id TEXT NOT NULL DEFAULT '';
UPDATE remote_watches SET launched_in_thread_id = thread_id;
`
