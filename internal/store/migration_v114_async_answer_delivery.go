package store

const asyncAnswerDeliveryV114SQL = `
DROP TRIGGER async_question_answer_insert;
DROP TRIGGER async_question_answer_update;
CREATE TRIGGER async_question_answer_insert AFTER INSERT ON items
 WHEN NEW.kind='user_text' AND json_type(NEW.meta,'$.provider_item_id')='text'
 AND length(trim(json_extract(NEW.meta,'$.provider_item_id')))>0
 BEGIN
 UPDATE async_questions SET state='delivered',user_item_id=NEW.id
 WHERE thread_id=NEW.thread_id AND send_id<>'' AND send_id=json_extract(NEW.meta,'$.sendId') AND state='submitted';
 DELETE FROM flush_queue_items WHERE thread_id=NEW.thread_id AND send_id<>''
 AND send_id=json_extract(NEW.meta,'$.sendId') AND EXISTS (
 SELECT 1 FROM async_questions q WHERE q.thread_id=NEW.thread_id AND q.send_id<>''
 AND q.send_id=flush_queue_items.send_id AND q.state='delivered' AND q.user_item_id=NEW.id);
 END;
CREATE TRIGGER async_question_answer_update AFTER UPDATE OF meta ON items
 WHEN NEW.kind='user_text' AND json_type(NEW.meta,'$.provider_item_id')='text'
 AND length(trim(json_extract(NEW.meta,'$.provider_item_id')))>0
 BEGIN
 UPDATE async_questions SET state='delivered',user_item_id=NEW.id
 WHERE thread_id=NEW.thread_id AND send_id<>'' AND send_id=json_extract(NEW.meta,'$.sendId') AND state='submitted';
 DELETE FROM flush_queue_items WHERE thread_id=NEW.thread_id AND send_id<>''
 AND send_id=json_extract(NEW.meta,'$.sendId') AND EXISTS (
 SELECT 1 FROM async_questions q WHERE q.thread_id=NEW.thread_id AND q.send_id<>''
 AND q.send_id=flush_queue_items.send_id AND q.state='delivered' AND q.user_item_id=NEW.id);
 END;
`
