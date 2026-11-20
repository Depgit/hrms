-- One response time for every approval stage (rulings: keep approver setup simple, no turnaround matrices).
CREATE TABLE approval_setting (
  id int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  sla_hours int NOT NULL DEFAULT 24 CHECK (sla_hours BETWEEN 1 AND 720)
);
INSERT INTO approval_setting(id) VALUES (1);
