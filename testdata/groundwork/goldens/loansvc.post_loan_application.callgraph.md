```mermaid
flowchart LR
    %% static call graph — scope: POST /loan-application; algo: rta
    %% 3 first-party nodes above tier 2 hidden as plumbing; pass --show-plumbing to include
    client_Gateway_Charge["client.Gateway.Charge ⚠"]:::fallible
    handler_App_Create["handler.App.Create"]
    origination_Evaluator_Evaluate["origination.Evaluator.Evaluate ⚠"]:::fallible
    origination_Evaluator_auditLog["origination.Evaluator.auditLog"]
    origination_Evaluator_disburse["origination.Evaluator.disburse"]
    origination_Evaluator_notify["origination.Evaluator.notify ⚠"]:::fallible
    store_Loans_InsertAudit["store.Loans.InsertAudit ⚠"]:::fallible
    store_Loans_InsertLedger["store.Loans.InsertLedger ⚠"]:::fallible
    external_payment_gw_POST__charge__id_(["payment-gw POST /charge/{id}"]):::external
    bus_bus_PUBLISH_disbursement_initiated{{"bus PUBLISH disbursement.initiated"}}:::bus
    bus_bus_PUBLISH_loan_approved{{"bus PUBLISH loan.approved"}}:::bus
    bus_bus_PUBLISH_loan_declined{{"bus PUBLISH loan.declined"}}:::bus
    bus_bus_PUBLISH__dynamic_{{"bus PUBLISH &lt;dynamic&gt;"}}:::bus
    db_db_INSERT_audit_log[("db INSERT audit_log")]:::db
    db_db_INSERT_ledger[("db INSERT ledger")]:::db
    blind_ExternalBoundaryCall(["⊥ ExternalBoundaryCall<br/>→ errgroup · effect-bearing"]):::blind
    frontier_dynamic_bus(["⌖ dynamic-bus<br/>frontier A"]):::blind
    frontier_severed_closure(["⌖ severed-closure<br/>frontier A"]):::blind
    frontier_severed_closure1(["⌖ severed-closure<br/>frontier A"]):::blind
    client_Gateway_Charge --> external_payment_gw_POST__charge__id_
    handler_App_Create --> origination_Evaluator_Evaluate
    origination_Evaluator_Evaluate --> client_Gateway_Charge
    origination_Evaluator_Evaluate --> origination_Evaluator_disburse
    origination_Evaluator_Evaluate --> origination_Evaluator_notify
    origination_Evaluator_Evaluate -. async .-> bus_bus_PUBLISH_disbursement_initiated
    origination_Evaluator_Evaluate -. async .-> bus_bus_PUBLISH_loan_approved
    origination_Evaluator_Evaluate -. async .-> bus_bus_PUBLISH_loan_declined
    origination_Evaluator_auditLog --> store_Loans_InsertAudit
    origination_Evaluator_disburse -. go .-> origination_Evaluator_auditLog
    origination_Evaluator_disburse --> store_Loans_InsertLedger
    origination_Evaluator_notify -. async .-> bus_bus_PUBLISH__dynamic_
    store_Loans_InsertAudit --> db_db_INSERT_audit_log
    store_Loans_InsertLedger --> db_db_INSERT_ledger
    origination_Evaluator_Evaluate -. blind .-> blind_ExternalBoundaryCall
    origination_Evaluator_notify -. blind .-> frontier_dynamic_bus
    origination_Evaluator_Evaluate -. blind .-> frontier_severed_closure
    origination_Evaluator_Evaluate -. blind .-> frontier_severed_closure1
    classDef fallible stroke:#c44,stroke-width:2px
    classDef db fill:#eef,stroke:#66a
    classDef bus fill:#efe,stroke:#5a5
    classDef external fill:#fef6e8,stroke:#c93
    classDef blind fill:#fde,stroke:#c33,stroke-dasharray:3 3
```
