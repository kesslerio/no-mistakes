# Custody release: PR-body follow-up

Include this disclosure in the existing PR body's Risk and Rollout section, preserving its generated Pipeline section.

Durable published-release provenance is deferred under the recorded R13 decision. Release and reconcile confirm publication only in their successful command response. Later status and sync still classify against the historical push binding; after releasing custody at a replacement published head, they can report advancement or rewriting, or offer synchronization toward the old head. Use the successful custody command's result to start a fresh run at the existing published branch. Historical pushed heads, errors and generations remain intact.

A separately authorized follow-up must durably distinguish a completed published release from ordinary custody recovery and failed release attempts before changing cached inspection or synchronization classification. A custody-return stamp plus release archives cannot establish completion: archives can survive a refused release, and ordinary recovery can adopt an unpublished head and write the same stamp. Preserve historical push provenance and test successful releases, refusals after archival, ordinary recovery, retries and changed evidence across restart.

The executable regression `TestReleasePublishedRefusalDoesNotCreditOrdinaryRecoveryAsPublished` covers the refusal followed by ordinary recovery for both release and reconcile. `TestReleasePublishedClassificationKeepsHistoricalPushProvenance` covers successful command responses at equal, ancestor, descendant and divergent published replacements without crediting later cached inspection as proof of release completion.
