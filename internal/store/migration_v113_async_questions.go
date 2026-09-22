package store

const asyncQuestionsV113SQL = `
CREATE TABLE async_questions (
 thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
 item_id TEXT NOT NULL,
 question_index INTEGER NOT NULL CHECK(question_index >= 0),
 title TEXT NOT NULL,
 options TEXT NOT NULL CHECK(json_valid(options)),
 state TEXT NOT NULL DEFAULT 'unanswered' CHECK(state IN ('unanswered','dismissed','submitted','delivered','restored')),
 answer TEXT NOT NULL DEFAULT '',
 send_id TEXT NOT NULL DEFAULT '',
 user_item_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 PRIMARY KEY(thread_id,item_id,question_index)
);
CREATE INDEX idx_async_questions_pending ON async_questions(thread_id,created_at,item_id,question_index)
 WHERE state IN ('unanswered','submitted','restored');
CREATE INDEX idx_async_questions_send ON async_questions(thread_id,send_id) WHERE send_id <> '';

CREATE TRIGGER async_question_answer_insert AFTER INSERT ON items
 WHEN NEW.kind='user_text' AND json_extract(NEW.meta,'$.provider_item_id') IS NOT NULL
 BEGIN
 UPDATE async_questions SET state='delivered',user_item_id=NEW.id
 WHERE thread_id=NEW.thread_id AND send_id<>'' AND send_id=json_extract(NEW.meta,'$.sendId') AND state='submitted';
 END;
CREATE TRIGGER async_question_answer_update AFTER UPDATE OF meta ON items
 WHEN NEW.kind='user_text' AND json_extract(NEW.meta,'$.provider_item_id') IS NOT NULL
 BEGIN
 UPDATE async_questions SET state='delivered',user_item_id=NEW.id
 WHERE thread_id=NEW.thread_id AND send_id<>'' AND send_id=json_extract(NEW.meta,'$.sendId') AND state='submitted';
 END;
`
