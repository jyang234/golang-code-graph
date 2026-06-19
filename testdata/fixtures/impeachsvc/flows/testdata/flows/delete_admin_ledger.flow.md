```mermaid
sequenceDiagram
    participant Client as Client
    participant impeachsvc as impeachsvc
    participant postgres as postgres
    Client->>impeachsvc: HTTP DELETE /admin/ledger
    impeachsvc->>impeachsvc: admin.purge
    impeachsvc->>postgres: DB postgres DELETE ledger
    impeachsvc->>postgres: DB postgres DELETE audit_log
```
