// Pure authoring assertions for the typed layer — no server needed.
// Run: node --import tsx test/authoring.mjs
// The Python mirror of this file is clients/python/tests/authoring.py.
import { defineAggregate, defineProjection } from '../src/index.ts'
import { z } from 'zod'

let failed = 0
const check = (name, cond, detail = '') => {
  console.log(`  ${cond ? '✓' : '✗'} ${name}${cond ? '' : `\n      ${detail}`}`)
  if (!cond) failed++
}

const Tasks = defineAggregate('task', {
  TaskCreated: z.object({
    owner: z.string(),
    title: z.string(),
    at: z.number().int(),
    estimate: z.object({ points: z.number().int() }),
  }),
  TaskMoved: z.object({ status: z.enum(['todo', 'doing', 'done']) }),
  TaskDeleted: z.object({}),
})

console.log('\n▶ TS authoring layer\n')

const tasks = defineProjection('tasks', Tasks, (on) => ({
  TaskCreated: on.TaskCreated.upsert((e) => ({ owner: e.owner, title: e.title, status: 'todo', created_at: e.at })),
  TaskMoved: on.TaskMoved.upsert((e) => ({ status: e.status })),
  TaskDeleted: on.TaskDeleted.delete(),
}))
check(
  'column inference from event schemas',
  JSON.stringify(tasks.def.columns) === JSON.stringify({ owner: 'text', title: 'text', status: 'text', created_at: 'integer' }),
  JSON.stringify(tasks.def.columns),
)
check('proxy path capture → wire rule', tasks.def.on.TaskCreated.set?.created_at === '$.at', JSON.stringify(tasks.def.on.TaskCreated))
check('delete rule compiled', JSON.stringify(tasks.def.on.TaskDeleted) === JSON.stringify({ op: 'delete' }))

// inc reads the payload: a total maintained by the fold, not by the app
const totals = defineProjection('board_totals', Tasks, (on) => ({
  TaskCreated: on.TaskCreated.inc((e) => ({ created: 1, age_sum: e.at })),
}))
check('inc proxy path → wire path', totals.def.on.TaskCreated.inc?.age_sum === '$.at', JSON.stringify(totals.def.on.TaskCreated))
check('inc literal → integer column', totals.def.columns.created === 'integer', JSON.stringify(totals.def.columns))
check('inc path column typed from the schema', totals.def.columns.age_sum === 'integer', JSON.stringify(totals.def.columns))

// a nested inc path: the server resolves it and adds a number, so the column
// must be typed from the leaf — not folded to text the way a nested set is
const points = defineProjection('board_points', Tasks, (on) => ({
  TaskCreated: on.TaskCreated.inc((e) => ({ points: e.estimate.points })),
}))
check('nested inc path compiles to a wire path', points.def.on.TaskCreated.inc?.points === '$.estimate.points', JSON.stringify(points.def.on.TaskCreated))
check('nested inc column typed from the leaf, not text', points.def.columns.points === 'integer', JSON.stringify(points.def.columns))

// the row key is a rule too: one row per owner, across every task stream
const perOwner = defineProjection('board_per_owner', Tasks, (on) => ({
  TaskCreated: on.TaskCreated.inc(() => ({ created: 1 }), { key: (e) => e.owner }),
  TaskDeleted: on.TaskDeleted.delete({ key: (e) => e.owner }),
}))
check('key proxy → wire path', perOwner.def.on.TaskCreated.key === '$.owner', JSON.stringify(perOwner.def.on.TaskCreated))
check('delete carries the same key', JSON.stringify(perOwner.def.on.TaskDeleted) === JSON.stringify({ op: 'delete', key: '$.owner' }), JSON.stringify(perOwner.def.on.TaskDeleted))

// a key that is not a payload path is rejected where it is written
let badKey = false
try {
  defineProjection('bad_key', Tasks, (on) => ({
    TaskCreated: on.TaskCreated.inc(() => ({ created: 1 }), { key: 'owner' }),
  }))
} catch {
  badKey = true
}
check('a key that is not a $. path is rejected', badKey)

// an inc path that does not name a number is a definition bug, caught here
let rejected = false
try {
  defineProjection('bad_points', Tasks, (on) => ({
    TaskCreated: on.TaskCreated.inc((e) => ({ n: e.title })),
  }))
} catch {
  rejected = true
}
check('inc path naming a non-number is rejected', rejected)

console.log(failed ? `\n❌ TS authoring ${failed} failed\n` : '\n✅ TS authoring passed\n')
process.exit(failed ? 1 : 0)
