package store

// Migration v160 marks each async question with the run of the app that
// recorded it. `run_id` is the opening Store's run identity (Store.run),
// written when RecordAsyncQuestions first inserts the question and never
// changed; a fork copies it, and nothing else writes it. A question whose
// mark is not the open store's run was asked before this run of the app and
// is the person's (AsyncQuestionGroup.AskedThisRun). A row from before this
// migration keeps the empty mark, which is no run's.
const asyncQuestionRunMigrationVersion = 160

const asyncQuestionRunV160SQL = `
ALTER TABLE async_questions ADD COLUMN run_id TEXT NOT NULL DEFAULT '';
`
