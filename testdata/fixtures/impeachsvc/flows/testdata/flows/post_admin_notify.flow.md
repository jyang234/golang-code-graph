sequenceDiagram
    participant Client as Client
    participant impeachsvc as impeachsvc
    participant Bus as Bus
    Client->>impeachsvc: HTTP POST /admin/notify
    impeachsvc->>impeachsvc: admin.notify
    impeachsvc->>Bus: PUBLISH ledger.purged
