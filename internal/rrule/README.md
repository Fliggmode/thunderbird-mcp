# Bounded recurrence engine

This directory contains teambition/rrule-go v1.8.2, pinned to commit
`429348ca4477691b08edb0200b7f837edd32414c`.
Source: https://github.com/teambition/rrule-go/tree/429348ca4477691b08edb0200b7f837edd32414c
The upstream MIT license and regression tests accompany the source.

Local changes add `RRule.Work` and `RRule.Err`. The iterator calls `Work`
before every frequency period, including periods with no matches.
A callback error terminates iteration synchronously. No worker goroutine survives
cancellation. This hook is necessary because upstream `UNTIL` checks occur only
when a candidate matches; an impossible rule can search through year 9999.

The calendar adapter rejects subdaily frequencies, which contain additional
internal search loops. It limits time expansions to one time per day.
It calls each rule separately: upstream `Set` stores only one RRULE.

When updating this copy, preserve the callback and run the upstream tests plus
`TestCalendarExpansionBounds`. Do not replace it with an unguarded iterator.
