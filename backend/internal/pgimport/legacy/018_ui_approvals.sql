-- Round 3 of the Approvals page: the improvement plan (PIP) has its own approval flow, so it shows in
-- the Approval flow editor and "Check a flow" (prototype MX.p_pip, WFNOTE.p_pip: "A PIP goes to the
-- HRBP (HRBP Corporate for corporate staff)"). Additive: inserted only where no p_pip flow exists.
INSERT INTO approval_matrix(wf, flow, cols, rows, no_response) VALUES
  ('p_pip', 'Operations', '["HRBP"]',
   '[{"id":"pip1","label":"Every plan","c":{"k":"always"},"on":true,"cells":{"HRBP":"A"}}]', 'escalate'),
  ('p_pip', 'Corporate', '["HRBP Corporate"]',
   '[{"id":"pip2","label":"Every plan","c":{"k":"always"},"on":true,"cells":{"HRBP Corporate":"A"}}]', 'escalate')
ON CONFLICT (wf, flow) DO NOTHING;
