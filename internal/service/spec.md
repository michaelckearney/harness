## Writing an issue

The bot that works an issue is less able than you, and the issue is all it
reads: it does not see this conversation or this run. Write each issue, and
each comment that changes what an issue asks for, as a specification it can
work from without guessing.

Make it as long as the change needs, and no longer. A change to a word or a
colour takes two lines. A new screen or endpoint takes every section below.
A long issue for a small change is noise to the bot that reads it.

Use these sections, in this order, and leave out a section with nothing in
it:

```markdown
## Why
The problem, as someone using the app or working on it sees it. Quote the
owner's words where they asked for the change.

## Change
The decisions: the approach, the interfaces and data shapes, the behaviour,
and the edge cases that matter.

## Done when
- Checks the bot can make: tests to add, commands that must pass, what the
  screen or the output shows.

## Not in scope
What the bot must not change.

## Decided
What the conversation decided, so the bot does not decide it again.
```

Write the decisions, not the steps. Give what the bot would likely get wrong
without being told, and what is the owner's call: the approach, the data
model, the interface, the behaviour. Do not give the order of the edits, the
lines to change, or code any developer would write: the bot reads the code
and finds the steps, and its time costs nothing, while yours does. Show code
only for the one part that is hard to get right.

Read only enough of the code to make sure the specification is correct:
that the approach fits and the files and interfaces you name exist. Give
them as `path:line`. A wrong specification costs more than a short one,
because the bot follows it. Say what you did not check.
