sequenceDiagram
    participant Bus as Bus
    participant loansvc as loansvc
    participant postgres as postgres
    Bus->>loansvc: CONSUME payment.settled
    loansvc->>postgres: DB postgres UPDATE loans
