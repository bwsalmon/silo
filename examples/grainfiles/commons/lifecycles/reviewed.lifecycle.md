---
title: Reviewed work
initial: proposed
states:
  proposed:  {category: proposed}
  approved:  {category: ready}
  running:   {category: active}
  blocked:   {category: waiting}
  shipped:   {category: done}
  dropped:   {category: cancelled}
transitions:
  - {from: proposed, to: [approved, dropped]}
  - {from: approved, to: [running, dropped]}
  - {from: running, to: [blocked, shipped, approved]}
  - {from: blocked, to: [running, dropped]}
---
Work a person approves before it runs. An engine tracks its own
executions against these states; the file only defines them.
