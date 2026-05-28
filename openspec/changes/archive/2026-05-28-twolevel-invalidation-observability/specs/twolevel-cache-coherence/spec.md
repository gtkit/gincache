## ADDED Requirements

### Requirement: Cross-instance L1 invalidation broadcast
TwoLevelStore SHALL support Redis Pub/Sub invalidation broadcast so successful `Set`, `Delete`, and `DeletePattern` operations can invalidate L1 caches in other instances.

#### Scenario: Delete invalidates another instance
- **WHEN** one TwoLevelStore instance deletes a key after both instances subscribed to the same invalidation channel
- **THEN** another instance that cached the key locally MUST remove that L1 entry before its local TTL naturally expires

#### Scenario: Set invalidates another instance
- **WHEN** one TwoLevelStore instance successfully updates a key after another instance cached the previous value locally
- **THEN** the other instance MUST remove its L1 entry and read the updated value from Redis on the next cache read

### Requirement: Explicit Pub/Sub client contract
TwoLevelStore SHALL require a `redis.UniversalClient` when L1 invalidation broadcast is enabled.

#### Scenario: Store without broadcast uses Cmdable client
- **WHEN** a caller creates TwoLevelStore without enabling L1 invalidation broadcast
- **THEN** the store MUST continue to accept the existing `redis.Cmdable` constructor client

#### Scenario: Broadcast option requires Pub/Sub capability
- **WHEN** a caller enables L1 invalidation broadcast
- **THEN** the broadcast option MUST require a Redis client type that exposes both publish and subscribe capabilities

### Requirement: Observable invalidation failures
TwoLevelStore SHALL expose a logger option for background invalidation failures.

#### Scenario: Publish failure is logged
- **WHEN** publishing an invalidation message fails
- **THEN** TwoLevelStore MUST log the channel, message type, value, and error

#### Scenario: Invalid payload is logged
- **WHEN** the subscriber receives a payload that cannot be decoded as an invalidation message
- **THEN** TwoLevelStore MUST log the invalid message error and keep listening

#### Scenario: Local invalidation failure is logged
- **WHEN** a subscriber receives a valid invalidation message but local L1 deletion fails
- **THEN** TwoLevelStore MUST log the failed key or pattern and error

#### Scenario: Unexpected subscription close is logged
- **WHEN** the subscription channel closes without an explicit store close
- **THEN** TwoLevelStore MUST log that the invalidation subscriber channel closed

#### Scenario: Expected close is not logged as an error
- **WHEN** the caller closes TwoLevelStore normally
- **THEN** TwoLevelStore MUST stop the subscriber without logging an unexpected subscription close

### Requirement: Bounded subscription startup
TwoLevelStore SHALL bound invalidation subscription startup with a configurable timeout and default to five seconds.

#### Scenario: Redis unavailable during startup
- **WHEN** invalidation broadcast is enabled but Redis cannot confirm subscription before the startup timeout
- **THEN** TwoLevelStore MUST disable invalidation broadcast for that store instance and log the startup failure

### Requirement: Bounded singleflight waiting
TwoLevelStore SHALL allow internal singleflight work for a key to be forgotten after a configurable timeout.

#### Scenario: Slow leader does not block later requests indefinitely
- **WHEN** a `Get` call for a key remains blocked past the singleflight forget timeout
- **THEN** a later `Get` for the same key MUST be able to become a new leader and complete independently
