-- 0003_agent.sql
-- Settings the agent loop reads. The schema itself needs nothing new: runs,
-- goal_plans, plan_tasks, messages, tool_calls, llm_calls and pushes were all
-- designed for this in 0001.

-- The base branch the work branch is cut from and rebased onto. Stored in
-- settings as well as on the project row so a new project picks up the right
-- default without being asked.
INSERT OR IGNORE INTO settings (key, value) VALUES ('git.base_branch', 'main');

-- Runaway guard, not a policy. The reviewer loop deliberately has no retry cap
-- because trusting the agent to self-manage has held up in practice; this only
-- stops a pathological loop from billing indefinitely while nobody is watching.
INSERT OR IGNORE INTO settings (key, value) VALUES ('agent.max_turns', '200');

-- Step budget for one end-to-end verification. Reaching it without a verdict is
-- itself a finding: the flow did not visibly complete.
INSERT OR IGNORE INTO settings (key, value) VALUES ('agent.e2e_max_steps', '12');
