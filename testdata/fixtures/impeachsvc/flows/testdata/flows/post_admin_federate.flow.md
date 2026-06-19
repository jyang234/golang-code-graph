```mermaid
sequenceDiagram
    participant Client as Client
    participant impeachsvc as impeachsvc
    participant postgres as postgres
    Client->>impeachsvc: HTTP POST /admin/federate
    impeachsvc->>impeachsvc: admin.federate
    impeachsvc->>postgres: DB postgres DELETE peer_ledger
```
