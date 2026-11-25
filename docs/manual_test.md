# Saltstayz HRMS: manual test checklist

Updated 9 November 2026 (My view without Approvals: Part C, D2, D7, D9, D18, D19, E2, E3, E7; Master update and
Edit on every master: D14). Tick each box as
you go (`- [x]`). When something fails, write a bug using the template at the end and put its number next to the
box.

How to read this file:
- Part A sets up the test database and the sign-ins.
- Part B is checks that apply to every page.
- Part C is what each role must and must not see.
- Part D goes page by page.
- Part E is end-to-end flows across roles: the most important tests.
- Part F is security and masking.
- Part G is the new **Set shift** feature (attendance rules per shift).

Keep the prototype (`Saltstayz HRMS (1).html`) open in another tab. A screen that looks or reads different from
the prototype is a bug, unless it is in the list in Part H.

---

## Part A. Before you start

### A1. Use a separate test database (not your real dev data)
The sample organisation adds 53 people and six weeks of punches. Keep it out of the `hrms` database.

```bash
make db
docker compose exec -T postgres psql -U hrms -c "create database hrms_manual"
cd backend
export DATABASE_URL=postgres://hrms:hrms@localhost:5433/hrms_manual
go run ./cmd/hrms migrate
go run ./cmd/hrms seed -sample
go run ./cmd/hrms serve          # API on :8080, leave it running
# in another terminal, from the repo root:
make web                         # open http://localhost:5173
```

- [ ] The API log ends with `HRMS API listening on :8080`.
- [ ] http://localhost:5173 opens the sign-in screen.
- [ ] `backend/.env` has `OTP_DEV_ECHO=true`, so one-time codes show on screen instead of going by SMS.

To start again from clean data, drop and recreate `hrms_manual`, then repeat the commands.

### A2. Sign-ins
EMP-1002 and EMP-1020 start with password `pass1234`. Everyone else signs in the first time with
**First time here? Activate with OTP**. Enter the ID; the code shows on screen; then set a password. The password
must meet the strength rules: use something like `Test#Pass2026` and the same one for everyone.

| Role | Employee | Name |
|---|---|---|
| HR Admin | EMP-1002 | Sandeep Rawat |
| CHRO | EMP-1001 | Anjali Verma |
| HRBP Corporate | EMP-1003 | Pooja Malhotra |
| HRBP | EMP-1004 | Meera Kulkarni |
| Operations Head | EMP-1005 | Aditya Mehra |
| City Head | EMP-1006 | Harish Gupta |
| Cluster Manager | EMP-1007 | Vivek Menon |
| Property Manager | EMP-1008 | Rakesh Bajaj |
| Line Manager (manager of EMP-1014, EMP-1020) | EMP-1009 | Anil Kapoor |
| Function Head | EMP-1010 | Rohit Khanna |
| Payroll | EMP-1011 | Sunita Rao |
| Payroll Manager | EMP-1012 | Deepak Arora |
| Finance Head | EMP-1013 | Mahesh Iyer |
| Employee, Contractual | EMP-1014 | Lata Nair |
| Employee, Permanent | EMP-1020 | Karan Khan |
| Employee, Retainer | EMP-1021 | Aarav Singh |

A person with a role also has the **Employee** view (their own pay and requests). Switch with the role picker in
the header. Use a second browser, or a private window, to be two people at once: one raises, the other approves.

---

## Part B. Checks for every page (do these on every page you open)

- [ ] **Matches the prototype:** same title, tabs, buttons, column names and order, wording and empty-state text.
- [ ] **No error banner, no broken layout.** Open the browser console (F12): no red errors.
- [ ] **Loading:** a slow page shows a loading sign, never a blank page.
- [ ] **Dialogs:** they open over the page, close with the ✕ or Cancel, and keep what you typed if saving fails.
- [ ] **Saving:** a green toast confirms each save. A refused action explains why in words (not "Error 400").
- [ ] **Dates and times** read like `04 Nov 2026` and `04 Nov, 14:22`, in India time.
- [ ] **Money** reads like `₹25,000`. Masked values read like `••••1234`.
- [ ] **Dark mode:** switch the computer to dark mode. Every page stays readable; no white boxes, no black text on dark.
- [ ] **Phone width:** narrow the window to about 390 px. Nothing runs off the side; tables scroll inside their box.
- [ ] **Refresh (F5)** keeps you on the same page, still signed in.

---

## Part C. Who sees what (sign in as each role, check the left menu)

Dashboard shows for everyone. Approvals shows in every role view, but **not in My view** (9 Nov): there each request
starts from its own page and is followed on the Dashboard (My requests). Tick a row when the menu is **exactly**
this, nothing more or less.

- [ ] **HR Admin:** Dashboard, Employees, Onboarding, Separation, Compensation, Approvals, Shift and roster,
      Attendance, Employee file, Payroll, Reports, Work policies, Masters, Bulk update, Access control, Employee dues.
- [ ] **HRBP:** Dashboard, Employees, Onboarding, Separation, Compensation, Approvals, Shift and roster,
      Attendance, Employee file, Reports, Employee dues.
- [ ] **HRBP Corporate:** Dashboard, Employees, Onboarding, Separation, Compensation, Approvals, Attendance,
      Employee file, Reports, Employee dues. *(No Work policies or Masters since 4 Nov.)*
- [ ] **Payroll** and **Payroll Manager:** Dashboard, Employees, Separation, Compensation, Approvals, Attendance,
      Employee file, Payroll, Reports, Employee dues. *(No Work policies since 4 Nov.)*
- [ ] **CHRO:** Dashboard, Employees, Separation, Compensation, Approvals, Attendance, Employee file, Payroll,
      Reports, Employee dues.
- [ ] **Finance Head:** Dashboard, Employees, Separation, Compensation, Approvals, Employee file, Payroll, Reports,
      Employee dues.
- [ ] **Property Manager, Cluster Manager, City Head, Operations Head, Function Head:** Dashboard, Employees,
      Separation, Approvals, Shift and roster, Attendance, Employee file, Reports.
- [ ] **Line Manager (EMP-1009):** Dashboard, Employees, Approvals, Shift and roster, Attendance, Employee file.
      Separation shows too, but only while one of their team has an exit case.
- [ ] **Employee / My view (EMP-1020):** Dashboard, Shift and roster, Attendance, My pay and profile, Separation,
      Employee directory. **No Approvals.** Typing `/approvals` in My view opens the Dashboard.
- [ ] A person with roles (e.g. EMP-1009) gets one header button per view: "My view" plus "<Role> view". My view
      shows only their own data and the menu above; it never shows another person's pay, Approvals or an Approve
      button. Switching to "Line Manager view" brings Approvals back, with their team's requests.
- [ ] For each role: typing a hidden page's address (e.g. `/masters` as Payroll) shows no data and no error flood.

---

## Part D. Page by page

Sign in as **HR Admin** unless a line says otherwise.

### D1. Sign-in
- [ ] Wrong password: a clear message; the ID stays filled in.
- [ ] Activate with OTP: the code shows; a weak password is refused, with the rule shown.
- [ ] The ID typed as EMP1002, emp1002 or EMP 1002 signs in as EMP-1002.
- [ ] Forgot password: same OTP flow; afterwards the old password no longer works.
- [ ] The header shows name · ID · Sign out. Sign out returns to sign-in, and Back does not reopen the app.
- [ ] A person with several roles can switch role in the header. The menu changes to match Part C.

### D2. Dashboard
**In My view (EMP-1020):**
- [ ] Tabs: Overview, Payroll, Attendance, Compliance only (no Workforce, no Approvals, no city/department filters).
- [ ] No "Headcount by city", "By employment type" or "By department" cards.
- [ ] Overview: my requests waiting, present this month, payroll month status, time with Saltstayz; "Needs attention"
      lists only my own items (policies, missing details, days to regularize, hold, notice, my requests).
- [ ] **My requests** (Overview, below the cards): every request raised by me or for me (leave, regularization,
      claims, detail and bank changes, resignation and its withdrawal, and items Payroll or HR raised about me such
      as a deduction), each with type and ID, dates or amount, raised date, status, **who it waits on** (name, stage
      and for how long) and the approval route with the current step. Waiting / Decided / All filters.
- [ ] An exit HR starts about me (termination, absconding, contract end, retirement) is **not shown until it is
      approved** (new, 11 Nov): not in My requests or the waiting count, not on my Separation page, not in Dashboard
      activity, and opening its link gives "not found". Starting a resignation meanwhile says "Your resignation
      cannot be sent right now. Please speak to HR". Once approved it shows as usual; a rejected one never shows.
- [ ] Clicking the "My requests waiting" card, or Open on the "My requests" item in Needs attention, scrolls to the
      list (no Approvals page).
- [ ] Open shows the request's detail and approval history (who approved, when, comments), read-only: no Approve or
      Reject. Cancel shows only on my own pending requests; a deduction Payroll raised about me has no Cancel (and
      the server refuses it: "This request was raised by …").
- [ ] "New request…" lists only what I can raise and opens its page: Leave (My leave), Attendance regularization
      (My attendance), Reimbursement claim (if my policy allows claims), Bank or profile change (My details),
      Separation.
- [ ] Messages from Payroll show at the top of the Dashboard, in every view (see the Payroll checklist part).
- [ ] Birthdays and work anniversaries: colleagues in my department and at my location, "This week" and "Next 30
      days"; anniversaries show "N years"; **no birth year or age** anywhere.
- [ ] Payroll: my last net pay and month, this year's earnings and TDS, next pay date, my pending claims and advances.
- [ ] Attendance: my own month (present, late, missing punches, leave, loss of pay).
- [ ] Compliance: my company policies (see E11) with a red count on the tab while one is pending, then "My details on
      record" showing only On record / Missing.

**In other views (HR Admin):**
- [ ] Tabs: Overview, Workforce, Payroll, Attendance, Compliance, Approvals.
- [ ] Overview alerts come in this order: Roster, Roster deviations, PIP approvals, Nominations, Reporting manager,
      Approvals, Details, Probation, Exits. Each alert's link opens the right page.
- [ ] City, department and type filters change the numbers on every tab.
- [ ] Payroll tab: figures for payroll roles. Others see "Payroll figures are visible to payroll roles only."
- [ ] **Attendance tab as Finance Head:** shows figures, not "You do not have access" (fixed 4 Nov).
- [ ] As Property Manager, the numbers cover only their property.

### D3. Employees
- [ ] The list shows people in your scope only (HRBP: their properties; Line Manager: their team).
- [ ] Search by name and code; filters by type, department, city and status.
- [ ] Open a person: every section loads. Salary, bank and ID values follow Part F masking.
- [ ] Job change with reason and effective date: today applies now; a future date shows as scheduled.
- [ ] Edit personal details (HR Admin, HRBP): mobile, email, birth date, address, emergency contact. Each change
      shows in the change history.
- [ ] Mark inactive needs a date and a reason. The person drops out of the active list.
- [ ] Export downloads a file. As Finance or Finance Head, download is refused by default.

### D4. Onboarding (HR Admin, HRBP) — in stages since 11 Nov
Every new hire starts as a shortlist and keeps one number: **SI-0001** (shortlist) → **OI-0001** (selected, offer) →
**JI-0001** (joined) → the **Employee ID** after joining. Nobody gets an Employee ID before joining.
- [ ] Tabs: Shortlist, Pipeline (count), Form setup (HR Admin). There is no "New joining" offer form any more.
- [ ] Shortlist: personal details (name, mobile, email, date of birth, gender, address), work experience rows
      (company, designation, from, to; Add a row; empty for a fresher), existing CTC (₹ a year), work city →
      reporting location, department → designation, hiring manager, optional résumé. The side card shows the next
      number (SI-0001) and the route with names: Line Manager (the hiring manager) → Function Head.
- [ ] Send with gaps: the list names each one (mobile 10 digits, under 18, an experience row that ends before it
      starts, a future month). The hiring manager cannot be you. The same mobile while that candidate is open is refused.
- [ ] Send: "SI-0001 sent for approval" and the candidate opens: large SI-0001, the trail SI → OI → JI → Employee ID.
- [ ] HRBP (north locations): a shortlist for a Bengaluru location is refused; Bengaluru candidates are not listed.
- [ ] Pipeline: segments Shortlisted, Interview, Offer, Joined (documents pending), With Payroll, Done, Closed with
      counts; each row shows the current ID, name, department, location, status and who it waits on. Offers raised
      before stages (OFR-…) are listed below, under "Offers raised before stages", and work as before.
- [ ] Selected candidate (see E1): Raise offer OI-0001 opens today's offer form prefilled from the shortlist (name,
      contact, department, designation, city, location; reporting manager = the hiring manager). Aadhaar is uploaded
      against the candidate. Send: "Offer OI-0001 sent for approval". Offer approval as before (band, budget).
- [ ] Offer approved: no employee and no Employee ID appear (Employees list, next code unchanged).
- [ ] Release, Mark accepted; Candidate declined (reason) closes the candidate as Declined.
- [ ] Mark joined: the date cannot be in the future or before the release. The number becomes JI-0001.
      Mark not joined (reason) closes the candidate as Not joined.
- [ ] Joining details and documents as before. With the default setting the last mandatory document (or detail)
      creates the Employee ID: "All mandatory joining documents are in. Employee ID EMP-… created." It is the next
      code of the series. Send to Payroll; Payroll and Payroll Manager verify; the person is active.
- [ ] Form setup › Employee ID: "When the mandatory joining documents are complete" (default) or "At joining", with
      the note that a person on a JI number is not in attendance, roster or payroll until the Employee ID exists.
      With "At joining", Mark joined creates the Employee ID at once.
- [ ] Rejected shortlist, Not selected, Declined, Not joined: Closed with the reason; none uses an Employee ID, and the
      next shortlist takes the next number (closed numbers are never reused).
- [ ] Employees › an inactive person › Rejoin › Start the shortlist: the shortlist opens prefilled from the old record
      (Saltstayz experience row, last manager, CTC from the last gross) with "Keep EMP-… / A new code". While it is
      open, Undo inactive and a second rejoin are refused.
- [ ] Form setup: change a field or a required document; the offer form follows it.

#### D4a. The joiner fills their own details (joiner's link, since 11 Nov)
Start the API with `OTP_DEV_ECHO=true` and no `SMTP_HOST` (emails go to the API log), then once with a real mailbox.
- [ ] Form setup: a **Filled by** column for fields and documents. PAN, bank, Aadhaar, date of birth are **Joiner**;
      shift, attendance mode, reporting managers, grade, pay are **HR** and cannot be changed (shown as plain "HR").
      Every document is **Joiner**. Change PAN to HR and back; it saves. Add a custom field in Personal: Joiner; in Job: HR.
- [ ] An accepted candidate (before Mark joined): **Joiner's link and joining details** opens the dialog with the
      **Joiner's link** panel at the top: email and mobile from the offer, "Not sent". Fields filled by the joiner have
      a small **Joiner** marker; documents too.
- [ ] Before the offer is released the panel says the link can be sent once the offer is released (no buttons).
- [ ] Without SMTP: the note "Email is not set up…", **Email the link** disabled (hover says why). **Copy link** makes
      the link and shows it; **Share on WhatsApp** opens WhatsApp with `91<mobile>` and a message with the link.
- [ ] **Copy link** again in the same browser tab reuses the link; in a new tab (or after **Send a new link**) it asks
      "A new link will be made, and the link sent earlier stops working"; the old link then says "This link has expired
      or is no longer in use".
- [ ] With SMTP: **Email the link** → "Link emailed to …"; the email "Complete your joining details for Saltstayz"
      arrives with the button, the address, the expiry date and the code note.
- [ ] On a phone (or 390 px wide): the link shows the company and "Email me the code" (or, without email, "HR will give
      you a code on the phone"). No name or offer details before the code.
- [ ] The code email arrives; a new code only after 30 seconds; a wrong code says how many tries are left; 5 wrong
      lock it ("Ask for a new code"); 3 locked codes lock the link ("Ask HR to send you a new link") and HR's panel shows
      "Locked". After 10 minutes a code has expired.
- [ ] Without email: HR's **Give a code by phone** shows a 6-digit code once; read it to the joiner; it opens the form.
- [ ] The form: "Hello <name>", offer number, type and joining date; sections Personal, Identity and PF / ESI, Bank
      account for salary, Benefits; required marks; no pay, managers, shift or other HR fields. UAN appears only after
      "Existing UAN available? Yes". Wrong PAN, Aadhaar, IFSC, account number or mobile: an inline message.
- [ ] Documents: Upload opens the camera or files (images and PDF); "Uploaded: <file>" after; over 5 MB refused.
- [ ] **Save and continue later**, close the tab, open the link again: a new code, the values are kept.
- [ ] **Submit to HR** with a required item missing: the list names it. With everything in: "Thank you". Opening the
      link again shows the thank-you page only.
- [ ] HR: the bell says "<name> submitted joining details (OI-…)"; the pipeline row shows **Joiner submitted**; the
      panel shows "Submitted by the joiner" with the times. The values are in the dialog, marked Joiner; HR changes
      one, fills Shift and Attendance mode and **Save**: with every mandatory item in, the Employee ID is created.
- [ ] **Reopen for the joiner** with a note: a new link (emailed when set up); the joiner sees "A note from HR" and
      their earlier values; the submitted link no longer opens.
- [ ] **Stop the link**: the joiner's page says the link is no longer in use. Sending to Payroll (or declining the
      offer) closes the link the same way.
- [ ] Audit log (Access control › Audit): link created / emailed, opened, saved (field names only), uploads,
      submitted, reopened, stopped, code by phone.
- [ ] As Payroll opening "View joining details" while verifying: no Joiner's link panel.

### D5. Separation
- [ ] Tabs and counts: Open, Show-cause, Approval, Notice period, Clearance, Full and final, Letters, Closed,
      Rejected or withdrawn.
- [ ] Start an exit as HR (every exit type). "Death or permanent disability" is in HR's reason list.
- [ ] Case view: stage, last working day, clearance by Manager, HRBP and Finance, exit interview, letters.
- [ ] Absconding: show-cause letters, waiting days, then approval.
- [ ] Cancel a case: it shows Cancelled.
- [ ] Settings: absconding days, letters, waiting days, notice recovery.

### D6. Compensation
- [ ] Tabs: Setup guide, Pay bands, Salary structure, Labour welfare fund, Budgets, Offer letter.
- [ ] Edit a pay band; an offer outside the band then shows the warning.
- [ ] A new structure version saves; mapping people to it happens in Payroll › Structure mapping.
- [ ] Change an LWF rule; the payroll preview for someone in that state changes.
- [ ] Budgets: edit a headcount and cost budget for a location.

### D7. Approvals
- [ ] Tabs: Requests (To approve, All open, Completed, For information), Approval flow, Approver names, Check a flow.
- [ ] New request: every request type opens its own form, and the approval route shows the real names.
- [ ] Your own request: no Approve button, and the text says "You raised this request, so someone else decides it".
- [ ] Approve and reject: rejecting needs a comment. The history shows who did what and when.
- [ ] Approval flow: each request type shows numbered rules per flow (Operations / Corporate; leave per leave group),
      each read as "If … → step 1 → step 2 · also tell …", with Otherwise last (no conditions; it cannot be moved,
      deleted or switched off).
- [ ] Approval flow: Edit a rule: change a condition (e.g. claim amount up to ₹5,000 → ₹4,000), add or remove a
      condition, add an approver from the list, reorder approvers with ◀ ▶, add someone under Also tell. Each change
      says Saved. A role cannot be both an approver and told in one rule; a rule needs at least one condition.
- [ ] Approval flow: Add rule (pick what it checks) adds it above Otherwise; move it up and down with ↑ ↓; switch it
      off (it is greyed and skipped); delete it (asks first).
- [ ] Approval flow › Test a request: enter grade / days / amount / exit type …, Test shows "Rule N decides: …" (or
      "No rule fits, so Otherwise decides") and the approvers in order. Moving a rule above another changes which
      one decides.
- [ ] Approvals › All flows at a glance: each flow has an "Approval required" tick (only who can edit flows sees
      the tick). Untick Shortlist › Operations and Interview › Operations (asks first). Shortlist an Operations
      candidate: it is approved at once and the candidate goes straight to Selected, ready for the offer; the
      request history says "Approved automatically: approval hierarchy is off". Untick Reimbursement claim ›
      Operations: an employee's claim is Approved at once and reaches the open month. Test a request says nobody is
      asked. A claim raised before unticking still waits; as HR Admin it shows "Skip approval" (reason required).
      Tick it again: a new claim waits for the approvers as before. The Approval flow tab has no tick (rules only),
      and Interview is not listed there.
- [ ] After a change, Check a flow shows the new route for a real employee, and All flows at a glance lists the
      rules in use in order with Otherwise last.
- [ ] Approver names: "No one mapped for…" lists the gaps. "Check overlaps" shows when two people hold one place.
- [ ] Approval flow, red "Approver names missing" box › **Bulk upload names** (26 Nov): Download template lists every
      missing name once (Role, Level, Place, Place Code, Where, Needed By, Employee). Employee is a drop-down of active
      employees "Name (EMP-…)". Pick two, leave the rest blank, Check file: two rows to apply, blanks "No change"; an
      inactive person is refused. Apply: the box loses those two, Access › Role mapping shows the place added to the
      person's mapping (or a new one). A role without Role mapping Edit does not see the button.
- [ ] **My view has no Approvals** (9 Nov): an employee's requests start from their own pages (D18, D19, D9) and are
      followed on the Dashboard (D2). Check a flow is no longer reachable in My view; each request form shows its
      own approval route instead.
- [ ] For information: completed requests show too.

#### D7a. Joining verification and bulk approval (Payroll EMP-1011, then Payroll Manager EMP-1012; since 11 Nov)
- [ ] Payroll view › Approvals › Joining approvals: open a joining verification (JV-…). Documents lists every joining
      document with Mandatory / Optional, Uploaded / Missing and the file name; **View** opens the file in a new tab.
- [ ] **Edit details** (only while it waits on you): change PAN to a wrong format → Save shows "PAN format looks wrong";
      clear a mandatory detail → "… is required". Put a right PAN and a new bank account → Saved; the history under
      Approval progress says "Details corrected by Sunita Rao: PAN … → …; Account number ••••…1234 → ••••…9012" (no full
      account or Aadhaar number). Employees › that employee › file shows the new PAN and bank account.
- [ ] **Replace** a document: the new file name shows, View opens the new one, the history says "Document replaced".
- [ ] As the Payroll Manager before Payroll has approved: no Edit details and no Replace, but View works. As an
      Employee (My view) the request and its files cannot be opened.
- [ ] **Download for bulk approval**: the Excel file has only the requests waiting on you; the header row is frozen;
      Decision is a list (Approve / Return); joining details are blue with Aadhaar and account masked; Documents says
      "N of N mandatory uploaded" and names any missing one; there is a "How to fill" sheet.
- [ ] Fill it: Approve one, Approve one with a corrected PAN, Return one with a comment, Return one without a comment,
      correct a bank name with no decision, type a wrong IFSC on one, edit a masked account number to "••••1234",
      leave one blank. **Upload decisions** → the preview shows will approve 2, will return 1, corrections only 1, and
      skipped with reasons (Return needs a comment; Correction not valid: IFSC format looks wrong; Type the full new
      Account number; No decision). Nothing has changed yet (open a request to check).
- [ ] Approve one of the previewed requests on screen, then **Confirm**: that row is skipped ("Not waiting on you any
      more" or "Already … since the download"); the others are approved (Next: Payroll Manager), returned or corrected.
- [ ] Tick two requests and **Approve selected (2)** → confirm → both move to the Payroll Manager. Sign in as the
      Payroll Manager: they are in Joining approvals to approve; approving makes the employee Active.
- [ ] Payroll approvals tab: the same Download / Upload / ticks work for leave, regularization, claims, advances,
      deductions, arrears, full and final and bank changes (tax proofs are not ticked).
- [ ] The audit log (`GET /api/audit` as HR Admin): one line per bulk run ("Bulk approval from Excel (Joining approvals): 2 approved, …") and one
      per correction.

### D8. Shift and roster
- [ ] The week grid opens on a department with people. The week arrows move by exactly one week.
- [ ] Change a cell to a shift, week off or holiday. A late change asks for a reason. Past dates are locked.
- [ ] A rest gap that is too short warns or blocks, per the policy.
- [ ] Publish (Property Manager, Cluster Manager, City Head): the roster shows as published.
- [ ] Deviations: mark OK or Not OK. OK fixes the roster to match what was worked.
- [ ] Discipline report: scores and bands; "By who made the change" is grouped by role.
- [ ] Consequences: issue a warning and a PIP. The PIP form names the approver ("goes to HRBP (Meera Kulkarni)").
      A withdrawal asks for a reason.
- [ ] As Employee (My view): only their own roster; **no Bulk upload tab** and no cell editing, Publish or pattern
      buttons, even for a manager in their own My view. In the manager's view these are back.

### D9. Attendance
- [ ] Tabs: Summary, Records (By employee, By date, Calendar), Missing punches, MIS upload, Punch machine setup.
- [ ] By employee: every day of the month, with status, in and out, and source.
- [ ] Regularize a day (title "New attendance regularization request"): in/out default to 09:00/18:00, and the route
      shows. After approval the day turns Present.
- [ ] MIS upload: download the template, fill two rows, upload. The check result shows, then apply.
- [ ] Punch machine setup: add a machine. The key shows **once**. Push and pull settings save; "Last pull …" shows
      after a pull (see G7 for a test push).
- [ ] Punch machine setup › Realtime India portal: without REALTIME_URL/USER/PASS it says "Account not set" and cannot be
  switched on. With them (restart): Pull now for one past day shows "Pulling…", then the result (employees with swipes,
  swipes, new punches) and the portal codes that match no employee. Records shows the punches. Pull the same day again:
  0 new punches. Give someone an Attendance code in Employee master update (last column), pull again: they match.
- [ ] Punch machine setup › Match portal codes: Fetch employee list shows "Fetching…", then "Fetched … N portal
  employees, M already have their code". Sure / Likely / Check / None counts; Sure rows show the portal code, name,
  department and why. Check rows offer a "Pick the right one…" list. Tick all Sure, Apply ticked: the codes are given
  (Employees › the person › audit shows "Attendance code … given from the Realtime portal match") and the rows leave
  the list. Download Sure and Likely for Bulk update: the file uploads through Bulk update › Employee master update
  unchanged. "Download the list" gives the portal employees who match nobody. As an Employee the section is not shown.
- [ ] Punch machine setup › Attendance alerts: everything Off at first. Turn on all five, Save; a time like "11" is
  refused. **Send a test to me**: the bell shows "Test of attendance alerts…" (and an email when SMTP is set up).
  Next day after 09:00: someone with only an in punch yesterday has "Missing out punch on …" in their bell, linking
  to My attendance. Someone who punched in after 09:15 has "In at …, … late". Someone with no punch 45 minutes after
  their shift start has "No punch-in yet today" and their manager a list; nobody on leave, week off or holiday gets
  one. After 11:00 the manager has "Your team today". With the Realtime pull on but failing, the card says the
  punch feed is stale and "Not sent today" lists it. The card shows today's counts; nothing is sent twice.
- [ ] As Employee: only their own attendance, no other names. Missing punches shows **Regularize** on their own days
      (the same dialog and route as My attendance).
- [ ] My view › Records › By date: From and To dates (default: 1st of the month to today), This month / Last month,
      one row per date, **no employee picker**. A period over 3 months is refused.
- [ ] Manager view: the employee picker shows only when more than one person reports to them; with one, it is
      preselected and hidden.

### D10. Employee file (HR Admin, Payroll)
- [ ] Pick a person: "Ready for <month> payroll?" with the readiness items (personal, bank, hold, tax, approvals,
      final attendance) and an overall state.
- [ ] Tabs: Overview, Personal, Bank, Tax, Attendance, Nominations, Pay and history.
- [ ] As HRBP: readiness shows, but pay figures are not shown (masking fix of 4 Nov).

### D11. Payroll (Payroll, Payroll Manager, HR Admin)
- [ ] Tabs: Salary calculator, Upcoming pay lines, Claims and changes, Full and final, Payroll run,
      Statutory and insurance files, Tax and Form 16, Payslip design, Structure mapping, Pay schedule and components.
      There is no Month close tab; a link with ?tab=close opens Payroll run.
- [ ] Salary calculator opens on grade G1, gross 19,900. Permanent, Retainer and Contractual each show the right breakup.
- [ ] Payroll run › "Before you run payroll": five closures (Attendance, Leave, Bank verification, Reimbursement,
      Deduction), each with its pending items split into Not raised and Waiting for approval, and whom each waits on.
      Close is enabled only when nothing is pending, and nothing can be set aside: an item clears only when the
      person it waits on acts (Reject waiting at cut-off stays for regularizations and leave). Run dry run stays
      disabled until all five are closed.
- [ ] Send message on a pending item: the person it waits on sees it under the bell and on their Dashboard, and it is
      marked done once the item is raised, approved or rejected. The item shows "Messaged <name>, <time>".
- [ ] Closing Attendance closure locks attendance: a regularization for a locked day is refused. Reopen (Payroll
      Manager or HR Admin, with a reason) unlocks it.
- [ ] Attendance closure: a missing punch ("Missing punch: in 09:00, no out time") has **Mark as LOP** next to Send
      message, and the dialog asks **Full day** or **Half day**; an absence has no choice (always a full day). See K1–K4.
- [ ] Bank verification closure: "No bank account on record" has **Hold salary**, and **Hold all (n)** sits above the
      list. A held person moves to **On hold (n)** and is not pending. See K5–K9.
- [ ] Payroll run: run the full sequence in E5.
- [ ] Upcoming pay lines: claims, arrears, advances and full and final rows. A pending PIP shows as "PIP · N days".
- [ ] Claims and changes: Reimbursement claims, Profile and bank changes, Arrears, Tax declarations, Nominations.
      Each sub-tab shows a count.
- [ ] Arrear reason types: Pay rate correction, Attendance or leave correction, Short payment in earlier month,
      Joining or exit adjustment, Other.
- [ ] Statutory files: PF, ESI, ECR, ESIC and insurance, each previewed with the prototype's columns.
      "Download CSV" works; ECR also has "Download ECR text".
- [ ] Tax and Form 16: deductor details, deposits, receipts, Form 16 view ("Place: … · Date: …"), release switch.
- [ ] Payslip design: each change saves by itself. A logo over 500 KB is refused.
- [ ] Pay schedule: pay day, cycle and the first payroll month, and **Bank upload: ICICI debit account number**
      (default 38605005062). See Part L.
- [ ] Payroll run: **Download full register** next to the payroll sheet download from the dry run on ("(draft)" before
      approval); once approved, a **Bank files** card with **Bank advice** and **ICICI bank upload**. See Part L.

### D12. Reports
- [ ] All 35 reports open, with the prototype's columns. The first column is "Code", not "Emp ID".
- [ ] Each report's filters are the prototype's. Leaving the dates empty covers all dates.
- [ ] Total rows show on headcount, register, cost, statutory, bank, retainer and TDS.
- [ ] Download CSV gives the same columns. Finance and Finance Head get no download by default.
- [ ] As HRBP: only their properties, and salary values masked.
- [ ] Bank advice is empty until the month is approved, with a note saying so.
- [ ] Bank advice also offers **Bank advice** (Excel) and **ICICI bank upload** for the chosen month, with the people
      the upload leaves out; Payroll register offers **Download full register, <month>**. See Part L.

### D13. Work policies (HR Admin only)
- [ ] Tabs: Policies, Coverage, Holiday calendars, Leave plans, Nominations, Exceptions.
- [ ] Policy tabs: Summary, Working time, Holidays, Attendance, Regularization, Leave plan, Blackout periods,
      Probation and notice, Applies to, History. There is no "Overtime and comp-off" tab (removed 8 Nov).
- [ ] Change a setting: History shows "Old → New" with your name. Older entries about removed settings (overtime,
      comp-off, break, late half day, extra-hours, rest and off-day deviations) are not shown.
- [ ] **Working time › Shifts and rest** has three fields, none with the ◦ marker, each with one help line:
      "Minimum rest between shifts (hours)", "When a roster breaks the rest gap" and "Notice before a shift change
      (days)". There is no "Split shifts allowed". The Attendance tab has no "Rest between shifts" card, and Roster
      discipline has no "Roster must be final this many days before the date".
- [ ] Rest of 11 h, "Do not allow the change": rostering N the night before an M day is refused with the rest
      message. Switch to "Allow it with a warning": it saves with the warning.
- [ ] Notice of 5 days: a roster change 4 days ahead asks for a reason and counts as a late change.
- [ ] There is no Night shift card (removed 24 Nov 2026, see Part AF): no night hours, no night shift allowance in
      payroll.
- [ ] Roster discipline: no "Working this many minutes more than the planned shift is flagged as extra hours".
      "Leave not allowed when the discipline score is below" sits under the points setting (0 = off).
- [ ] What counts as a deviation: no extra-hours, short-rest or "working on a weekly off, holiday or leave day"
      settings. The "In this policy a deviation is" card lists only arrival and early exit.
- [ ] A Reimbursements card is at the end of Working time, with "Employees can claim reimbursements".
- [ ] Holidays tab: no "Working on a holiday" setting. Attendance tab: no "Hours worked and late arrival" card.
- [ ] Summary: no overtime and no holiday-work line; a "Shifts and rest" and a "Discipline" line; Reimbursements
      line still there.
- [ ] **Probation and notice › Probation and notice by grade:** columns Grade, Probation (months), Notice in
      probation (days), Notice after confirmation (days), Employees. Blank means the policy default. Set a grade's
      probation to 3 months: an employee of that grade who joined 4 months ago shows "completed" on the employee
      record, the confirmation-due report and alert use the 3 months, and their resignation uses the notice after
      confirmation. 0 months = no probation. More than 24 is refused.
- [ ] Applies to: map by location, department, grade and so on. Coverage shows who has which policy. The note reads
      "When a person matches more than one policy: Individual (employee) > Department + Location > Department +
      Cluster > Department + City > Department + State > Location > Cluster > City > State > Department > Designation >
      Grade > Employment type" (Department + place since 24 Nov, Part AD). Map one person's city to policy A and their department
      to policy B: Coverage and the employee record show A via City.
- [ ] **Discipline score start:** someone who joined on the 21st of a 30-day month shows "Started at 33 (joined
      21 Nov)" in My time › Roster discipline, Shift and roster › Discipline and the discipline score report, and is
      Good while the score is 30 or more.
- [ ] **Leave below the score:** set the limit to 70 and mark Not OK deviations until an employee is below it. Their
      leave request is refused for any leave type, by anyone, with "Leave is not allowed while the discipline score is
      below 70 (now …)", shown in the dialog before sending. As HR Admin, raising it for them shows "Allow this request
      anyway": with a reason it goes through, and the Activity log has the reason. A part-month person's limit is
      scaled the same way as the score. 0 switches it off.
- [ ] Payroll run and payslips have no Overtime line and no comp-off; months already published or locked are unchanged.
- [ ] Holiday calendar by city; leave plan types, cover and limits.
- [ ] Nominations: the policy editor; "To verify" and "Not yet submitted" lists.

### D14. Masters (HR Admin only)
- [ ] Tabs: **Master update**, Regions, States, Cities, Clusters, Locations, Departments, Designations, Grades, Shifts,
      Qualifications, Employment types, Attendance modes, Change reasons, Company. The page still opens on Cities.
      Master update shows only to someone who can change a master (Masters: organisation or job at "edit").
- [ ] Lists are in the order items were created, not alphabetical.
- [ ] Add an item; make one inactive with a date and a reason. There is no delete.
- [ ] Company: the short name and address, used on letters and payslips.
- [ ] **Edit** on every row (Shifts keep their Edit, which opens Set shift). It opens the Add form's fields, filled in:
      Name and the parent, grades, approval flow or used for. A parent list shows active items and the current one
      even if it is inactive ("(inactive)"). Save shows "Saved" and the list changes. Designations have no separate
      Grades button any more: the grades are in Edit.
- [ ] Rename grade G1 (Edit): employees on G1, the designations that offer it, the pay bands, a work policy mapped to
      G1, "Probation and notice by grade" and the approval rows for G1 all show the new name. A job change to the new
      name works. The Activity log has "Changed grade" with the references updated.
- [ ] Rename a department: its work policy mapping, a Function Head mapped to that department (they still see its
      people), onboarding documents limited to it and a company policy's audience follow the new name.
- [ ] Rename a designation whose name another department also uses: both keep the work policy.
- [ ] Move a location to another cluster: before saving, the note says its N active employees move with it; after
      saving their city, state and region follow. Move a designation that active employees hold to another
      department: refused with "N active employees hold this designation in …; move them first with Bulk update,
      or add a new designation".
- [ ] Cannot change: an employment type's name (the Name box is locked with the reason); a shift's code.
- [ ] The change reasons the app writes itself (Transfer, Excess payment recovery, Absent without notice, the six
      separation reasons) and the attendance mode Biometric **can be renamed** (new, 11 Nov): Edit shows "The app uses
      … itself" and the Used for box is locked. Rename Excess payment recovery: a deduction of that type still needs
      no hearing; Bulk update salary recoveries use the new name. Rename Absent without notice: an absconding case
      holds the salary with the new name, and Payroll › Holds suggests it for someone absent. Rename Terminated: an
      approved termination records the new name. Rename Biometric: employees on it move to the new name, and a new
      joiner without a mode gets it.
- [ ] **Master update › Download with current data**: one workbook with "How to fill" first, then Regions, States,
      Cities, Clusters, Locations, Departments, Grades, Designations, Shifts, Qualifications, Employment types,
      Attendance modes, Change reasons; header bold and frozen; Status, Inactive from, Inactive reason, then a grey
      "Row ID (leave as is; blank for a new item)" last. Parent and reason columns have drop-downs that warn (a new
      item typed in is allowed); Status, Approval flow and Used for stop. No budgets (Bulk update › Budgets) and no
      shift rules (Set shift). **Download blank format**: the same with no rows. Both are in the Activity log.
- [ ] Upload the current-data workbook unchanged and **Check file**: every sheet "No change", 0 changes, 0 errors.
- [ ] In the blank format add region East, state Odisha (Region East), city Bhubaneswar (State Odisha), a cluster and
      a location under it, in one file. Check shows 5 new; nothing is saved until **Apply 5 changes**. Then the
      location's line in Locations reads "… › Bhubaneswar › Odisha › East".
- [ ] Rename North to "North India" on Regions only (the States sheet still says North): Check shows 1 change and no
      errors; after Apply, Delhi's region is North India.
- [ ] Errors are listed by sheet and row and are skipped on Apply: a repeated row ("Appears again, first on line N"),
      an unknown Row ID, a city name used in two states without "Name (State)", a parent that does not exist or is
      inactive, an unknown or inactive grade, an Inactive reason that is not a Master data reason, a past Inactive
      from date, a changed shift code or employment type name.
- [ ] Status Inactive with a Master data reason and a blank date retires the item today, with "N active employees
      still use it"; a future date shows "(scheduled)"; setting it back to Active shows "Reactivated".
- [ ] Leave a sheet out: nothing in that master changes, and the result lists it under "Not in the file".
- [ ] A role with Masters: organisation at edit and Masters: job at view gets only the five organisation sheets; a
      job sheet uploaded by them reads "Not read: changing these masters needs the Masters: job right".
- [ ] A CSV file is refused: "A CSV file holds one sheet only". More than 5,000 rows or 10 MB is refused.

### D15. Bulk update (HR Admin)
- [ ] **Choose a template** shows the templates in three groups, wrapped (no row that scrolls sideways):
      **Employees** (Employee master update, Mark inactive (separation), Import employees, Personal details, Uniform sizes),
      **Pay and dues** (Salary breakup, Pay bands, Budgets, Opening advances, Opening uniform) and **Leave and time**
      (Opening leave balances). The chosen one is dark; the page address gets `?tpl=<key>`, and opening
      `/bulk?tpl=openuni` (or `openadv`, `uni`, `lvopen`) opens that template directly.
- [ ] At phone width (390 px) the groups wrap and the page does not scroll sideways; the check result lists each row
      as a block.
- [ ] Employee master update has a **Work Policy** column with a drop-down.
- [ ] Upload with one good row and one bad row. The bad row is named with the reason, and the good row applies.
- [ ] Results read "· from <date> (scheduled)" or "(back-dated)", with the reason.
- [ ] Every template's **How to fill** sheet (Download template) explains the columns; Opening uniform, Opening
      advances and Opening leave balances have an example row.

#### D15a. Opening uniform and opening advances (staff moved onto the HRMS mid-year; since 13 Nov)
On a fresh sample seed the first HRMS deduction month is **October 2026** (the open payroll month; the next month once
its Deduction closure is closed). The blue note on both templates names it.
- [ ] **Opening uniform**, paste and Check file:
      `Employee ID,Issued month,Uniform amount (₹),Instalments,Items / note,Recover`
      `EMP-1020,Sep-2026,5000,2,Shirt and trousers,Yes` (the owner's case)
      `EMP-1016,2026-10,3000,,2 shirts,` · `EMP-1014,09/2026,2000,2,,` · `EMP-1020,2025-10,1200,2,,` · `EMP-9999,2026-09,1000,,,`
- [ ] EMP-1020: "Recovered before HRMS: ₹2,500: Sep 2026 ₹2,500", "HRMS deductions: Oct 2026 ₹2,500",
      "Refund: ₹5,000 in April 2027 · 6 months after the last deduction".
- [ ] EMP-1016 (blank instalments): the Setup split, Oct ₹1,500 and Nov ₹1,500, nothing before the HRMS.
- [ ] EMP-1014 (contractual): "recorded only", no deduction. A row with Recover `No` reads the same.
- [ ] `2025-10`: refused, "already refunded before the HRMS; nothing to load". EMP-9999: not found. A month in the
      future, more than 24 months back, an amount of 0 or over 1,00,000, or 13 instalments: refused.
- [ ] Apply. Employees › EMP-1020 (or Employee file › Uniform): the issue shows **Opening balance** and the note,
      "September 2026: ₹2,500 · recovered before HRMS", "October 2026: ₹2,500", "₹2,500 still to recover" and
      "Expected ₹5,000 in April 2027". Payroll › October shows a "Uniform recovery (2 of 2)" deduction of ₹2,500.
      Employee dues › Uniform lists it, with no stock taken from any store.
- [ ] Upload the same file again: every row reads "Already loaded as UNI-…". Nothing is added twice.
- [ ] Mark an employee inactive (or give a last working day before the refund month) and load opening uniform for
      them: the refund reads "not paid … left before the refund month"; their full and final shows "Uniform deposit
      refund not paid".
- [ ] **Opening advances** (Payroll › salary advance limits: raise the multiple to 3 first, or use smaller amounts):
      `Employee ID,Advance amount (₹),Advance given month,Deduction start month,Monthly deduction (₹),Deduction closing month,Reference / reason`
      `EMP-1020,30000,2026-06,2026-07,5000,2026-12,Voucher 2231` · `EMP-1016,12000,2026-07,2026-08,5000,2026-10,`
      `EMP-1017,10000,2026-09,2026-09,3000,2026-11,x`
- [ ] EMP-1020: recovered before HRMS ₹15,000 (Jul, Aug, Sep), HRMS deductions Oct, Nov, Dec ₹5,000, balance ₹15,000.
      EMP-1016: ₹10,000 recovered, ₹2,000 in October (the remainder). EMP-1017: "Deduction closing month should be
      December 2026 …".
- [ ] Apply: Employee dues › Advances lists both, tagged **Opening balance**, "given June 2026"; Skip next and Recover
      balance work. A second open advance for the same person and an amount above the company limit are still loaded
      (old advances, already given); a start month before the given month and a row with nothing left to recover are refused.
- [ ] Employee dues › Advances › Opening advances still works as before and links to Bulk update › Opening advances.
- [ ] **Opening leave balances**: the help says "Give the balance as at the date, including all leave accrued up to
      that month. From the next month, leave keeps accruing by the leave plan (e.g. CL 12 a year adds 1 a month)."

### D16. Access control (HR Admin)
- [ ] Roles and rights: change one right; the History records it. Sensitive groups are Full, Masked or Hidden.
- [ ] Role mapping: map a role to a person with locations or departments. "Approver for" fills in.
      "Check overlaps" shows a clash.
- [ ] Check access: pick a person, then see their pages, sensitive groups and the employees they can see.

### D17. Employee dues (HR Admin, Payroll; Finance, Finance Head and CHRO view only)
- [ ] Tabs: Assets, Uniform, Uniform sizes, Inventory, Advances, Salary holds, Deductions, Setup.
- [ ] Add stock with serials; issue an asset; take it back; issue a uniform with a recovery plan.
- [ ] **Uniform sizes (since 13 Nov).** Employees › open a permanent person › *Uniform sizes* › **Add sizes**: Shirt /
      T-shirt, trouser waist, trouser length, shoe size from the lists; Save. The record shows them; Change history has
      one line per size. As the employee (My pay and profile) they cannot change them; as a Line Manager the block is
      read-only. The payroll file has the same block on its **Uniform** tab.
- [ ] Setup › **Uniform sizes**: add 5XL to the shirt list and save; it appears in the sizes dialog and on the joining
      form. Setup › Uniform items › **Size by**: Shirt = Shirt / T-shirt size, Trousers = Trouser waist and length,
      Shoes = Shoe size.
- [ ] Uniform › Issue a uniform: pick the person; the note shows their sizes and each item's size is pre-filled
      (trousers as W and L); change one; issue. The list shows "2 × Shirt (M)" etc., the recovery months and
      "Expected … in <month>".
- [ ] **Uniform sizes** tab: counts by size per item, trousers by waist and length, location and department filters,
      "No sizes on record (N)", **Download Excel** (sheets By size, Trousers, Employees, No sizes).
- [ ] Setup › Uniform recovery › **Refund basis**: *After months of employment*, 12; the note and the issue form say
      "after 12 months of employment". Issue to a recent joiner: the refund month is when they complete 12 months.
      Switch back to *After the last deduction*: earlier issues keep their refund month.
- [ ] Mark a person with a scheduled refund inactive (last working day today): the Uniform list says **Not
      refunded**; Payroll › Full and final shows "Uniform deposit refund not paid" (info) and the recovery still
      due. Undo inactive: the refund is expected again.
- [ ] As the employee: My view › Payroll › **My uniform** shows the sizes, what is still to recover and the expected
      refund month.
- [ ] Onboarding › Form setup: section **Uniform** with four fields, Joining Optional, Filled by Joiner. A joiner's
      link shows them; after joining the sizes are on the record. Bulk update › **Uniform sizes** downloads current
      data and takes an upload.
- [ ] Payroll can "Add an asset" to the central store.
- [ ] An advance and a deduction go for approval. Put a salary on hold, then release it.
- [ ] As Finance Head: everything shows, but there are no action buttons.
- [ ] **Advances first for Payroll (since 12 Nov).** As Payroll (EMP-1011) or Payroll Manager (EMP-1012) the page opens
      on **Advances**, the first tab, with a line: "N advance requests are waiting for approval" (or where employees
      ask) and a link to Approvals › Payroll approvals (Payroll) or Approvals › Requests (Payroll Manager). HR Admin,
      HRBP and Finance see the tabs in the old order and no line.
- [ ] **Opening advances (Payroll, Payroll Manager, HR Admin).** Advances › Opening advances › **Download template**:
      eight columns (Employee ID, Employee name, Advance date, Original amount, Balance still to recover, Monthly
      recovery, Recovery starts, Reference / reason) and a "How to fill" sheet. Fill two good rows and some bad ones
      (unknown ID, a contractual employee, balance above the original, balance 0, monthly 0, a start month before the
      open month, the same employee and reference twice). **Upload opening advances**: the preview lists each row as
      Will add or Skipped with the reason and the total balance; nothing is added yet. **Confirm**: the good rows are
      added, tagged **Opening balance** in the list, with recovery lines only (no "Salary advance" payment line) and the
      last month taking the remainder. Upload the same file again: every row is "Already loaded as ADV-…".
- [ ] Skip next and Recover balance work on an opening advance. The Activity log shows "Opening advances uploaded:
      N added, N skipped, N failed".

### D18. My pay and profile (EMP-1020, then EMP-1014, then EMP-1021)
- [ ] Tabs: Payslips, Tax declaration, Form 16, My leave, My attendance, Nominations, My roster discipline,
      Reimbursements, Salary advance (permanent staff), My details. Contractual and Retainer staff see only the tabs that apply to them.
- [ ] Apply for leave (My leave): the balance shows "as on the From date". A long leave asks for a document. The
      approval route shows with names, as the old Approvals dialog did. The request lists "Waiting for <stage>
      (<name>)" and can be cancelled until approved.
- [ ] My attendance: **Regularize** on a day with a missing or no punch (not on a day already waiting) opens "New
      attendance regularization request" with the system record and the route.
- [ ] Tax declaration: add an item and upload a proof. "Submitted 04 Nov, 11:38" shows the time. The proof shows
      its file name.
- [ ] **Tax regime once a year:** choose New or Old and submit. The choice locks: "You chose the new regime for
      FY 2026-27. You can choose again from 1 April 2027." Declarations and proofs stay editable. Changing the
      regime is refused.
- [ ] Payroll (Claims and changes › Tax declarations › Review) can "Unlock regime choice" with a reason; the employee
      sees who unlocked it and can choose again once; it locks again on the next submit. The employee can't unlock
      their own, and the unlock shows in the Activity log.
- [ ] Reimbursement with a bill. The claim lists who it waits on and has Cancel request while pending.
- [ ] **Salary advance (since 12 Nov; EMP-1020, permanent).** My pay and profile › **Salary advance** (also from
      My view › Payroll › "Request a salary advance", and New request… on the Dashboard). The form shows the rules:
      up to 1 × monthly gross, one advance at a time, paid with the open payroll month (the next one once Deduction
      closure is closed), recovery from the month after, 1 to 12 instalments. Typing an amount shows "₹X a month for N
      months from …" and the approval route. Above the limit the form says "You can ask for up to …". Send: it lists as
      Pending with Cancel request, and in My requests. A second request is refused while the first is open. Cancel it,
      ask again. EMP-1014 (contractual) has no Salary advance tab.
- [ ] Approve it as Line Manager, then HRBP. As Payroll, open it in Approvals: **Change recovery** (pay month, 1 to 12
      instalments, first recovery month) → Save the change: the history says "Recovery changed by … : 3 instalments …
      → 4 instalments …". Approve: the advance and its recovery lines are in Upcoming pay lines.
- [ ] Payroll › Pay schedule and components › **Salary advances asked for by employees**: change "Most an employee can
      ask for" (0 = no limit) and "Advances open at a time"; the employee's form follows.
- [ ] **Reimbursement switch:** Work policies › (policy) › Working time › Reimbursements › "Employees can claim
      reimbursements" = No. Employees on that policy no longer see the Reimbursements tab, and a claim they raise is
      refused; HR or Payroll can still raise one for them. Set it back to Yes: the tab returns.
- [ ] My details: request a mobile or bank change; it waits for Payroll, and the list under it says who it waits on.
- [ ] **No Separation card** on any My pay tab.

### D19. Separation (My view)
- [ ] Before resigning: "Start resignation" with the notice from the policy; the toast names the first approver.
- [ ] After: the My exit card, and Case progress (stepper, stage, last working day and days to go, approval chain,
      exit interview, full and final, clearance by owner, assets to return).
- [ ] While the resignation waits for approval: a note says who it waits on, with **Withdraw before approval**
      (cancels it; the case shows Withdrawn) and "See it in My requests".
- [ ] **No** Withdraw button on the My exit card (prototype). While on notice a note says until when it can be
      withdrawn and has **Withdraw resignation**: the Approvals form (Employee, Request: Withdraw resignation only,
      current last working day read-only, reason, note) with the approval route and names, and the same rules
      (only a resignation, only in the notice stage, within the policy's days). After sending, the note lists the
      request "Waiting for <stage> (<name>)" with Cancel request (see E7).
- [ ] Past the withdraw-by date, or once clearance has started, the note says it can no longer be withdrawn and
      shows no button. "Change last working day" stays with HR (ask the HRBP).
- [ ] No mention of Approvals anywhere on the page.

### D20. Employee directory (My view)
- [ ] Shows only my department's active colleagues. "Around me": my reporting manager on top, me and my peers, my
      reports. Clicking a card re-centres; "Up to …" goes back up.
- [ ] "Whole department" shows the full tree; search by name, designation or location.
- [ ] Managers from another department show greyed, labelled "Other department".
- [ ] Cards show name, designation and location only: no employee code, salary, mobile, email, birth date or IDs.

---

## Part E. End-to-end flows (the most important)

Use two browsers. Note each request ID (e.g. LV-3005) as you go.

### E1. Hire to first payroll (onboarding stages)
- [ ] HR Admin shortlists a Sales Executive at Sector 29 Hub, hiring manager Anil Kapoor (EMP-1009): SI-0001.
- [ ] EMP-1009 (Line Manager view) has it under Approvals › To approve, with the experience, CTC and résumé.
      Approve: "Approved. Next: Function Head". Rohit Khanna (EMP-1010, Function Head view, Sales) approves:
      "Fully approved. Interview is next."
- [ ] EMP-1009's bell: "Interview waiting for you: … (SI-0001)". The interview in To approve has Selected and
      Not selected (no Approve), a date and feedback (required). HR Admin, the HRBP and the Function Head cannot record it.
- [ ] Selected: "… selected. Offer ID OI-0001". HR's bell: "… was selected at the interview: SI-0001 → OI-0001".
- [ ] HR raises OI-0001 (D4). Each offer approver approves in turn; still no Employee ID.
- [ ] Release, Mark accepted, Mark joined today: JI-0001. Joining details, then PAN, photo and bank proof: the last one
      creates the Employee ID (next in the series). Send to Payroll; Payroll, then Payroll Manager verify:
      "… is verified by Payroll and is now active for payroll."
- [ ] The candidate shows Done and the trail SI-0001 → OI-0001 → JI-0001 → EMP-…; the person appears in the payroll run.
- [ ] My view (any of these people) never shows a candidate.

### E2. Leave
- [ ] EMP-1020 applies for 2 days of leave in My pay and profile › My leave. It shows in Dashboard › My requests,
      waiting on Anil Kapoor (Line Manager). The Line Manager (EMP-1009) gets it under To approve.
- [ ] Approve. The days show as leave in My attendance and in Attendance › By employee, and the balance drops.
- [ ] Apply in a blackout period, or past the staffing limit: refused, or a warning, per the policy.
- [ ] Reject another leave with a comment. The employee sees the comment in Dashboard › My requests › Open (history).
- [ ] A leave still waiting can be cancelled from My leave or My requests; the days return to the balance.

### E3. Missing punch → regularization
- [ ] Pick a day with a missing punch for EMP-1020 (Attendance › Missing punches).
- [ ] The employee raises a regularization from My pay and profile › My attendance, or Attendance › Missing
      punches (Regularize), or the manager from their view; it is approved: "Hours will count in payroll."
- [ ] The day turns Present, and it drops off Missing punches.
- [ ] After Attendance closure is closed (Payroll run checklist), a regularization for that month is refused.

### E4. Roster → deviations → discipline
- [ ] Line Manager builds next week's roster; Property Manager publishes it.
- [ ] A late change (inside the lead time) asks for a reason and counts in the discipline report.
- [ ] A day worked on another shift shows as a deviation. Mark it OK: the roster updates. Mark another Not OK: the
      score drops.
- [ ] Issue a PIP. The approver named on the form gets it in Approvals.

### E5. Payroll month (the money flow)
- [ ] Payroll run checklist: clear the five closures the real way, then Close each. Employees raise a regularization
      or leave for each day listed Not raised (HR opens an exception when the window has passed); each approver decides
      what waits on them; Payroll verifies or rejects bank changes inline. There is no button that sets an item aside.
      Waiting regularizations and leave can still be rejected at the cut-off.
- [ ] Payroll (EMP-1011): Payroll run → Dry run. Spot-check 3 people: paid days, earnings, PF, ESI, TDS, net.
- [ ] Adjust one person with a reason; the row shows "edited by Payroll".
- [ ] Mark verified. It stays disabled until all five closures are closed and the pilot is cleared.
- [ ] Payroll cannot approve. Payroll Manager (EMP-1012) approves: "Approve and update register".
- [ ] The Payroll register card updates at once, without a page refresh (fixed 4 Nov).
- [ ] Publish payslips. EMP-1020 sees the payslip in My pay; its figures match the run.
- [ ] Reports › Bank advice now lists net pay. Statutory files for the month produce rows.

### E6. Claims, arrears, advances, holds
- [ ] EMP-1020 claims ₹3,000 with a bill: Line Manager, then Payroll. Paid in the next run.
- [ ] A claim of ₹30,000 adds the Payroll Manager and Finance steps (bands at ₹5,000 and ₹25,000).
- [ ] An arrear goes to the Payroll Manager (and the Finance Head above ₹25,000). It shows in Upcoming pay lines,
      then on the payslip.
- [ ] An advance with instalments: the recoveries show month by month.
- [ ] A hold: net pay leaves the bank advice until it is released.

### E7. Resignation, change, withdraw, exit
- [ ] EMP-1020 resigns from My view › Separation (Start resignation). Dashboard › My requests shows it waiting on the
      first approver. Approved, the case moves to Notice period.
- [ ] HR raises "Change last working day" in Approvals › New request › Resignation. Approved, the case date changes.
- [ ] EMP-1020 raises **Withdraw resignation** from My view › Separation (the only request they can raise while on
      notice; same form, route and rules as Approvals). It shows in My requests. Approved, the case shows
      Withdrawn, and any notice recovery is cancelled.
- [ ] Another resignation runs to the end: clearance by all three, exit interview, letters, full and final in
      payroll, case Closed.

### E8. Tax year
- [ ] Employee declares (old or new regime) and uploads proofs.
- [ ] Payroll reviews each proof amount (Tax declarations sub-tab; the count drops).
- [ ] TDS shows in the run; record a deposit; finalise.
- [ ] Release Form 16. The employee sees it under Form 16 (Part A and Part B).

### E9. Nominations
- [ ] Employee submits gratuity and PF nominations with documents.
- [ ] Payroll verifies them; the count in Claims and changes › Nominations drops; the employee sees "Verified".

### E10. Bulk and reports round trip
- [ ] Bulk-change 3 people's department from next month. They show as scheduled; on the date they apply.
- [ ] Reports › Employee change history lists the three changes, with reason and effective date.

### E11. Company policy acknowledgement (new)
- [ ] HR Admin: Work policies › Company policies › add a policy (title, version, date in force, PDF, applies to
      everyone or chosen departments/locations/types, new joiners must sign Yes/No). Save as draft: employees don't
      see it. Publish: it is due.
- [ ] Employee (My view › Dashboard › Compliance): the policy shows "Not submitted", with Download and a red note
      "Your salary for <month> will be held until <policy> is approved".
- [ ] Payroll run (dry run) for that month: the employee is held, reason "Company policy not acknowledged: <title>";
      the run checks show "Company policies acknowledged" with the count held; the Employee file shows "Company
      policies: 1 pending, with <manager>".
- [ ] Employee uploads the signed copy (PDF, JPG or PNG): status "Waiting for manager".
- [ ] Reporting manager (EMP-1009 for EMP-1020) sees it in Approvals › To approve, with the policy and the signed copy
      side by side. **Return** with a comment: the employee sees the comment and uploads again.
- [ ] Approve: status Signed; the hold is released automatically; recalculating the run pays the person.
- [ ] HRBP can act on it from All open; the history says "(HRBP, on behalf of Line Manager)". HR Admin can too.
- [ ] New version: due again for everyone; Retire: no longer due, and its holds are released.
- [ ] The setup list shows Signed / Waiting for manager / Returned / Not submitted counts; the policy opens to the
      people list.
- [ ] Reports › Company policy acknowledgement lists each person and policy with status and dates.
- [ ] Manager and HR dashboards show "N people have company policies pending".

### E12. Documents in Google Drive (new, 11 Nov)
Use a throwaway Google account and folder, or a test Workspace, never the company's real Drive while testing. One-time
Google Cloud setup: README › "Google Drive for uploaded documents". Sign in as HR Admin (EMP-1002).
- [ ] **Not set up** (no `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`): Masters › Company › "Document storage (Google
      Drive)" says Not connected and lists the five Google Cloud steps with the exact redirect URI and a Copy button
      (through the web app: `http://localhost:5173/api/drive/callback`). Upload any document (an employee's PAN):
      it works as before, and the card shows "1 file on the server, until Drive is connected and a folder is set".
- [ ] **Connect**: with the client ID and secret in `backend/.env` and the API restarted, **Connect Google Drive**
      opens Google's sign-in; choose the account, allow Drive. Back on the card: "Google Drive connected as
      <email>", Connected, "by Sandeep Rawat (EMP-1002), <date>". Cancel at Google instead: a red note "Google sign-in
      was cancelled, so Drive is not connected". The Activity log has "Google Drive connected" with the email and no
      token.
- [ ] **Folder link**: paste a folder link in any form (`…/drive/folders/<id>`, `…/drive/u/0/folders/<id>?usp=sharing`,
      `…/open?id=<id>`, or the bare id) and **Save folder**: "Folder saved: <name>" and "Saving in <name>". A link to a
      file says "This link is to a file, not a folder"; a folder the account can only view says "… can only view this
      folder. Give it edit access"; a folder not shared with the account says "No folder with this link is visible to
      <email>". A shared drive folder works.
- [ ] **Waiting files go up**: within a minute "Waiting" is "Nothing waiting", "In Drive" counts it, "Last upload"
      has a time. In Drive: `EMP-1020/EMP-1020 PAN card.pdf` (an employee from before onboarding stages).
- [ ] **Naming**: a candidate's documents (Onboarding, SI-0001): `SI-0001/SI-0001 Aadhaar card.pdf`,
      `SI-0001/SI-0001 PAN card.pdf` (the label is the one in Onboarding setup; rename "Aadhaar card" to "Aadhaar" there
      and the next upload is `SI-0001 Aadhaar.pdf`). Once SI-0001 has an Employee ID, the employee's own uploads
      still go to `SI-0001/`. A nomination document: `EMP-1020/EMP-1020 Nominee Aadhaar copy 1 (Gratuity
      nomination).pdf`. A signed policy: `EMP-1020/EMP-1020 <policy> v1.0 signed.pdf`. A deduction proof: `EMP-…
      Deduction proof <date>.pdf`. The payslip logo: `Company/Payslip logo.png`; a company policy file:
      `Company/<policy> v1.0.pdf`. An old offer without an employee: `OFR-1001/OFR-1001 Aadhaar card.pdf`.
- [ ] **Replace**: upload the PAN again for the same person: the new file is `EMP-1020 PAN card.pdf` and the earlier
      one is renamed `EMP-1020 PAN card (replaced 11 Nov 2026).pdf`. Nothing is deleted in Drive.
- [ ] **Open**: open the PAN from the employee's documents: it opens (streamed from Drive) for HR Admin and for the
      employee; another employee still gets "You do not have access". `backend/data/uploads` no longer has the file.
- [ ] **Folder deleted in Drive**: delete `EMP-1020/` in Drive and upload another of EMP-1020's documents: a new
      `EMP-1020` folder is made.
- [ ] **Drive down or no internet** (turn the network off for the API): an upload still saves at once; the card shows
      it waiting and "Last problem: Google Drive is not answering right now; it will try again". The document opens
      (from the server). With the network back, it goes up within a few minutes, or at once with **Try now**.
- [ ] **Disconnect**: confirm; the card says Not connected; Google's "Third-party apps with account access" no longer
      lists the app. Opening a document already in Drive says "Reconnect Google Drive to open this file". New uploads
      wait. Connect again: they go up and old documents open again.
- [ ] **Move existing**: on a database with documents from before this change, the card shows "Before Drive: N files
      … still on the server" and **Move N existing files to Drive**. Nothing moves until it is clicked; then they go
      up (replaced versions with "(replaced …)") and leave the server. The Activity log has "Moving N existing files
      to Google Drive".
- [ ] Only HR Admin sees the card (Masters › Company is HR Admin only); the API refuses everyone else
      (`/api/drive…` gives 403). No response, log line or Activity log entry contains the refresh or access token.

### E13. Documents from existing employees (new, 12 Nov)
Fresh `seed -sample`, API restarted (the sweep runs on start). Everyone in the sample came in without onboarding.
- [ ] **Settings** (HR Admin, Onboarding › Form setup › "Documents from existing employees"): Collect documents on,
      Days to upload 90. Change to 60 and Save: every open window's last day moves 30 days earlier. Put it back.
- [ ] **Who gets it**: Employees › **Document collection** lists every active employee missing a mandatory document,
      "0 of 5", "90 days left", Open. Someone who joined through onboarding (an SI/JI joiner) is not listed. Import one
      employee (Bulk update › new employees): they appear with today as the start.
- [ ] **Employee** (EMP-1020, My view): the bell has "Please upload your documents … by <date>"; the Dashboard shows
      "Upload your documents"; **My documents** lists Form setup's documents with Mandatory / Optional, Missing, and
      the days left. Upload a PDF for Aadhaar and a photo for PAN: "Uploaded, waiting for HR check". A .txt, an .svg,
      an .html renamed .pdf or a file over 5 MB is refused. View opens the file.
- [ ] **HR check** (HR Admin): open EMP-1020 in the tracker: **Verify** Aadhaar (Verified, by Sandeep Rawat), **Send
      back** PAN with a reason (a reason is required). As EMP-1020: PAN shows "Sent back" with the reason and a bell
      message; Aadhaar has no Replace button; upload PAN again: waiting for HR check.
- [ ] **HR uploads on behalf**: in the drawer, Upload a missing document: the file says "Uploaded by HR".
- [ ] **Extend / reopen**: tick EMP-1020, **Extend (1)** by 10: "+10 days" and the new last day; the employee's bell says
      so. To see a closed window, set the window start 100 days back in a test database
      (`db.doc_window.updateOne({employee_id:'EMP-1020'},{$set:{start:'<date>'}})`): My documents says "closed … Ask HR
      to reopen it", Upload buttons are gone, the tracker shows "Overdue 10 days"; Extend by 5 reopens it from today.
- [ ] **Filters and Excel**: Overdue, Due in 7 days, To check, Complete count and filter; **Send reminder** sends one bell
      message (a second the same day is skipped); **Export to Excel** downloads the tracker with the filter applied.
- [ ] **Reminders** run on their own at 30, 15, 7 and 1 days left, once each; when a window ends with documents
      missing, the HRBP (or HR Admin) gets one bell message.
- [ ] **Scope**: as HRBP (EMP-1004) only Gurugram and Delhi people are listed; Payroll sees the tracker read only (no
      Verify, Extend or Upload); Line Manager has no tab; as an employee, `/api/doc-collection/EMP-1020` gives 403.
- [ ] **Audit log**: Uploaded, Verified, Sent back, Extended / Reopened, Sent a document reminder, the setting change;
      only document and file names, never contents.

---

## Part F. Security and masking (try to break it)

- [ ] **HRBP (salary Masked):** salaries show `₹ ••••`, no digits at all. The same in reports, the CSV, the employee
      file and the change history (pay rows show the change without amounts).
- [ ] **Line Manager:** salary, bank, Aadhaar/PAN and medical show as Hidden or are absent.
- [ ] **Payroll:** salary, bank and IDs in full; nominee medical details masked.
- [ ] **CHRO:** IDs and bank Hidden; salary Full. On another person's payslip (and Payslip design's preview) no PAN,
      UAN or bank shows.
- [ ] **Line Manager:** a report's bank change request in Approvals and in the employee file's Requests shows no
      account number; the change history shows no personal or pay rows; a joining verification's documents are
      refused.
- [ ] Nobody can approve a request they raised, or one about themselves.
- [ ] Open another employee's page by changing the ID in the address bar while signed in as an Employee: refused.
- [ ] As Property Manager, open a person from another property by URL: refused.
- [ ] A down-graded role (remove a right in Access control): the page disappears after refreshing, and the API
      refuses it too.
- [ ] Every download (report, employee export, bulk template) appears in Reports › Activity log as "Downloaded …",
      with who and when.
- [ ] Wrong password 5 times: "Too many wrong attempts. Try again in 15 minutes, or reset your password with OTP."
- [ ] A wrong OTP 3 times: "Too many wrong OTP entries. Request a new OTP." An OTP older than 5 minutes is refused.
- [ ] A session ends after 12 hours; the next click returns to sign-in.
- [ ] Without `OTP_DEV_ECHO` (as in production), ten failed sign-ins in a row from one browser (any IDs): the next try
      says "Too many attempts from this device. Wait 15 minutes". With the echo on, the limit is 300. After an OTP is verified, `POST /api/auth/password` without the `verifyToken` is refused.

---

## Part G. Set shift (new, 6 Nov)

The client's "Set Shifts" screen is in **Masters › Shifts**. Rules set on a shift override the work policy; a rule
left empty follows the policy, as before. Use a test shift, e.g. code `TS`, 10:00–19:00, and roster one person
(EMP-1020) on it for a week.

### G1. The form
- [ ] The Masters › Shifts list has "Effective from" ("From the start", or "Next from …" when a change is waiting)
      and a rules column ("Policy rules" when none are set).
- [ ] Add shift and Edit open a full-width "Set shift" form, with sections Shift, Attendance rules and Version history
      (Punch rules and Single punch rules were removed on 24 Nov 2026, see Part AF). Read-only roles get View instead of Edit.
- [ ] Each rule field is empty, with "Policy: …" under it showing the value it falls back to. Once a rule is set,
      a "Use work policy" button appears and empties it again.
- [ ] Save asks for the effective date and an optional note.
- [ ] The note at the bottom says Consider NA, Configurable attendance calculation and IS consider first entry are
      not available yet.
- [ ] Shift ending next day is pre-set from the times. Yes with end after start (or No with end before start) is refused.
- [ ] Absent rule > Half day rule, or Half day rule > Full day rule: refused, with the reason.
- [ ] Wrong formats (`25:00`, `abc`) are refused. HHH:MM accepts `120:00`.
- [ ] Read-only for anyone without the Masters edit right.

### G2. Effective date (versions)
- [ ] Change the end time from tomorrow. Today's attendance keeps the old time; tomorrow's uses the new one.
- [ ] Version history lists both versions, with date, change and who did it.
- [ ] An effective date inside a locked (closed) payroll month is refused, with a message.
- [ ] A future-dated change shows on the shifts list once its date arrives.
- [ ] The roster and attendance show the version in effect on each date.

### G3. Attendance rules (rostered on TS; regularize or punch to set the hours)
Set Absent < 04:00, Half day < 06:00, Full day > 08:00.
- [ ] 3 h worked → Absent.
- [ ] 5 h → Half day.
- [ ] 7 h → "Short hours" on the day views. With no adjustment on, the month register settles it as Half day, with
      a note.
- [ ] 8 h 30 → Present (or Late if the arrival was late).
- [ ] Max Hrs 10:00, worked 12 h → counted as 10 h.
- [ ] There is no "Actual hours without break" and no "OT rule" (removed 8 Nov): hours always run from the in punch
      to the out punch, and no overtime is paid. A shift saved earlier with either still opens and saves.
- [ ] Attendance based on "Punch": any in punch makes a full day. "Shift": hours count only inside 10:00–19:00.

### G4. Weekly and monthly adjustment
- [ ] Weekly adjustment on, 10:00. Mon 7 h (short 1 h), Tue 10 h (extra 2 h, office hours 08:00): Monday stays Present.
- [ ] The same with the weekly cap 00:30: Monday becomes Half day.
- [ ] Shortfall inclusion "Weekly-off working hours" on: hours worked on a week off cover a short day. Off: they don't.
- [ ] Monthly adjustment covers what the weekly one left, up to its cap.

### G5. Punch rules
Removed from shifts on 24 Nov 2026: the work policy decides (Part AF).

### G6. Night shifts
The existing Night shift (`N`) keeps the old behaviour (a night split across two dates) until it is saved once
from the new form. Open N, leave "Shift ending next day" on Yes, and Save from the first day of the open month;
that also rebuilds that month's punch-machine days.
- [ ] Before saving N: a 22:00–06:00 night's in and out land on two different dates (e.g. "Missing out punch").
      After saving: one complete day on the date the shift started.

Or make shift `NS` 22:00–06:00, ending next day = Yes, and roster EMP-1020 on it.
- [ ] Punch in 21:55 on day 1, out 06:05 on day 2 → **one** Present day on day 1 (not "Missing out punch").
- [ ] Two nights in a row: each night pairs its own in and out; no punch is used twice.
- [ ] A regularized night keeps the approved times.

### G7. Sending test punches (for G5 and G6)
Add a machine in Attendance › Punch machine setup and copy its key. Then:

```bash
curl -s http://localhost:8080/api/punches -H 'Content-Type: application/json' -H 'X-Device-Key: <key>' \
  -d '{"punches":[{"employeeId":"EMP-1020","time":"2026-11-10 21:55","direction":"IN"},
                  {"employeeId":"EMP-1020","time":"2026-11-11 06:05","direction":"OUT"}]}'
```
- [ ] The reply counts the punches accepted. Sending the same punches again does not double them.
- [ ] An unknown key is refused.

### G8. Questions waiting on the client
Not built until they answer:
- [ ] "Consider NA": what does it do?
- [ ] "Configurable Attendance Calculation": what does Yes switch on?
- [ ] "IS Consider First Entry In Last Entry Out": when unticked, should hours be the sum of each in-to-out pair?

Built as below; confirm each is what they mean:
- [ ] **Short hours:** the band between the Half day and Full day rules. The day counts present; the missing hours
      (measured against Office hour time) must be covered by extra hours in the week, then the month, oldest day
      first, in full or not at all; a day not covered becomes a half day.
- [ ] **Based on Punch:** an in punch with no out counts as a full day. The late mark still applies.
- [ ] **Shortfall inclusion:** all hours worked on a weekly off, holiday or optional holiday count as extra hours.
- [ ] **Punch window:** 4 hours either side when the work policy sets no limit (Part AF).
- [ ] **Break minutes** on the shift are for display only; no break is taken off the hours worked (decided 8 Nov).

---

## Part I. Payroll fixes of 13 Nov (retest)

1. Change a bank account for someone in an approved month: the row is held and listed on Salary holds. Verify the account: the next run pays "Held salary released, <month>".
2. Verify a run, then change a gross or upload attendance: Approve is refused as stale. The person who verified or edited the run cannot approve it.
3. Upload attendance or apply leave for a date in a published month: refused with "already paid; raise an arrear or recovery".
4. On the Attendance or Leave closure, use "Mark as LOP" with a reason on a leaver's or contract worker's absence: the item clears and the audit log shows it.
5. ECR: a basic of ₹25,000 gives EPS wages 15,000, EPS 1,250 and EDLI wages 15,000. In the ESI file, wages are the earned gross and days are whole numbers.
6. Payslip: net pay in whole rupees, a "Rounding off" line, and the words match. The register rows add up, and the bank file shows the same net.
7. Gross revision from the 15th: that month pays the old gross for days 1–14 and the new gross from the 15th.
8. F&F: PL is encashed when "Encashment on exit" is on. A negative F&F shows "Owed by employee", and the no-dues certificate is blocked until it is recovered or waived.
9. Line Manager or HRBP opens a separation case: no F&F amounts. A masked salary reads "₹ ••••".
10. Tax › Opening TDS: download the template, Check file, Save. Finalising before the proof window ends asks for a reason.

---

## Part J. Fixes of 13 Nov, other areas (retest)

1. Bank change by EMP-1014: as Line Manager EMP-1009 and Property Manager EMP-1008 the account number is not in
   Approvals or the employee file; as HRBP it is masked; Payroll sees it in full.
2. Activate a new person with OTP in the browser: it still works end to end (the page sends the verification token).
3. HR Admin › Access control: add a role copied from Property Manager and map it to someone. Change one of its rights:
   the audit log entry has before and after.
4. Onboarding: as Payroll there is no Release offer or Candidate declined button. As HRBP, releasing still works.
5. Masters › Employment types: no Add form; making Permanent inactive is refused with the reason. Making Resigned
   (change reasons) or Biometric (attendance modes) inactive is refused too.
6. Masters › Shifts: a shift coded WO, PH or L is refused.
7. Work policies: Saturdays set to something not in the list (through the API) is refused; a mapping to EMP-9999 is
   refused; a second holiday on the same date of a calendar is refused.
8. Bulk update: download Salary breakup, Pay bands and Budgets current data and upload them unchanged: no changes,
   no errors. An Excel date cell in any display format (13-Nov-2026, 13/11/2026) reads as that date.
9. Import employees: gender "Robot", a mobile or Aadhaar already on file, a child's date of birth, Inactive with a
   future last day, a code with "/" are each refused on their row.
10. On notice, the employee's Withdraw resignation goes for approval; the direct withdraw is refused for them. HR's
    direct withdraw cancels the waiting request.
11. A night-shift worker regularizes 22:00 to 06:00: accepted.
12. Tax › Remind: each person who has not declared gets a bell notice. Raising 80D self above 25,000 is refused.
13. A person who has left cannot raise any request in My view, except a reimbursement claim within 15 days of the
    last working day.
14. Still waiting for the owner: leaving up to 10 minutes early on an 8-hour shift (early-out grace) still counts as
    short; "Applies from" on a work policy is recorded but the change takes effect at once.

---

## Part K. Payroll checklist: missing punches as loss of pay, and holding a salary with no bank account (14 Nov)

Sign in as Payroll (EMP-1011) or HR Admin (EMP-1002), Payroll › Payroll run, "Before you run payroll", open month.

1. Attendance closure › a "Missing punch" row › **Mark as LOP**. The dialog offers **Full day** (chosen) and **Half
   day**, and needs a reason of at least 5 characters. Choose Full day: the toast says "1 day marked as loss of pay",
   the row is gone, and the employee's calendar (Attendance › the person's month, or their My view) shows the day
   as **Loss of pay**, 1 day.
2. Another missing punch › Mark as LOP › **Half day**: "1 day marked as loss of pay (1 as a half day)". The calendar
   shows **Loss of pay (half day)**, 0.5 loss of pay; the punched half is worked. In Leave the request reads Loss of
   pay, half a day, raised by you, with the reason in its history.
3. With absences and missing punches still listed, **Mark all as loss of pay (n)** asks one Full day / Half day choice
   for the missing punches ("Absences stay a full day"). After it, nothing is Not raised in Attendance closure.
   Reject waiting at cut-off if anything waits, then **Close** works.
4. Run the dry run: the person whose missing punch was marked Full day has 1 more day of loss of pay than before,
   the half-day one 0.5 (the run's Paid days column and the payslip preview). The audit log has "Marked 1 missing
   punch as a half day as loss of pay at the payroll cut-off (attendance closure)".
5. Bank verification closure (to get an item, import an employee without bank details, DOJ in the open month):
   "No bank account on record" › **Hold salary**. The reason is prefilled "No bank account on record"; clearing it
   to fewer than 5 characters disables the button. Hold: "Salary put on hold"; the person moves to **On hold (1)**
   with the hold number (HLD-…) and the month, and the closure shows Done when nothing else is pending. **Hold
   all (n)** holds everyone listed in one go.
6. Employee dues › Salary holds lists the hold (reason "No bank account on record", the month, "that month only").
   The employee's My view › Payroll says "Your salary for <month> is on hold: No bank account on record". The
   employee file (Payroll › Payroll run › File) shows "Salary hold … for <month>".
7. Close the five closures and run the month to approval: the held row has the On hold pill, the run's Net payable
   card says "1 on hold, ₹… not included", and the bank advice file has no line for the person. The payslip is
   still issued (the salary is calculated).
8. Next month, with still no bank account: the person is listed again under Bank verification, with "Salary for
   <last month> is on hold (HLD-…). Holding it again adds <this month> to the same hold." Hold salary again: the same
   hold now reads "<last month> to <this month>" on Salary holds.
9. The employee raises a bank change (My pay and profile › My details) and Payroll verifies it (inline on the
   closure, or Claims and changes): the hold shows Released by System ("Bank account ending 5678 on record"), and
   the open month has a pay line "Held salary released, <month>" for each held month already run, with the held net.
   An account added by Bulk update or import instead is picked up by the next dry run in the same way.
10. Payroll can still release the hold by hand in Salary holds, as before.
11. Refused: as an Employee, HRBP or Line Manager (the actions need Payroll's run rights at Edit, like Close); after
    the closure is closed ("Attendance closure is closed", "Bank verification closure is closed"); in a month that is
    verified or not the open month.

## Part L. ICICI bank upload and the full salary register (14 Nov)

Sign in as Payroll (EMP-1011) or Payroll Manager (EMP-1012). Both files need **Export payroll files** at Edit and the
role's download right (the old register's rule); every download is in the audit log ("Downloaded <file name>").

1. Payroll › **Pay schedule and components** › **Bank upload**: the field shows 38605005062. Enter 8 digits, or a
   letter: Save stays disabled (the server also refuses: "must be 9 to 18 digits, digits only"). Enter 12 digits
   starting with 0 and Save: "Debit account saved", the value keeps its leading 0, and the audit log has "Bank upload
   debit account updated" with the old and new numbers. A role without Payroll settings at Edit sees it read-only.
2. Payroll run, month in **Dry run** or **Verified**: **Download full register (draft)** gives
   `full-salary-register-Oct-2026-DRAFT.xlsx`; row 1 reads "DRAFT: October 2026 not yet approved; figures may
   change", the header is row 2 and stays frozen with Emp ID and Name when scrolling. There is no Bank files card yet,
   and `/api/payroll/<month>/icici-upload.xlsx` answers "… is not approved yet".
3. The full register's columns, in order: Emp ID, Name, Designation, Department, Grade, Employment type, City,
   Location, Date of joining, Last working day, PAN, UAN, ESIC no, Bank, Account number, IFSC; Days in month, Paid
   days, LOP days, Days outside employment; Monthly gross (structure), CTC a month; Basic, HRA, Special allowance,
   Bonus, Variable pay, Arrears, Reimbursements, Salary advance, Uniform refund, Held salary
   released, Other earnings, Total earnings; Employee PF, Employee ESI, LWF (employee), TDS, Meal, Accommodation,
   Advance recovery, Uniform recovery, Asset recovery, Other recoveries, Total deductions, Rounding off, Net pay;
   Employer EPF, Employer EPS, EDLI, EPFO admin charges, Employer ESI, Employer LWF, Medical insurance, Gratuity
   provision, Total employer cost, Cost to company for the month; On hold, Hold reason, Remarks. The last row is
   **Total** with the number of employees. Pick three people: earnings add up to Total earnings, deductions to Total
   deductions, Total earnings − Total deductions + Rounding off = Net pay, and Net pay is the register's and the
   payslip's.
4. Approve the month. The register card at the bottom lists the month with **Full register**, **Bank advice** and
   **ICICI bank upload**; the full register is now `full-salary-register-Oct-2026.xlsx` with no DRAFT line.
5. The **Bank files** card: "ICICI upload: N payments, ₹…, (n within ICICI as FT, m by NEFT), from debit account …,
   narration "Oct 26 Salary"". To see the problems list, change one person's IFSC to `HDFC1234` and remove another's
   bank account before the run (Bulk update or the employee file): both are listed with name, code and reason, and
   the file has a second sheet "Not included" with the same people and amounts.
6. Open `ICICI-bank-upload-Oct-2026.xlsx`: first sheet has exactly the 13 headers PYMT_PROD_TYPE_CODE, PYMT_MODE,
   DEBIT_ACC_NO, BNF_NAME, BENE_ACC_NO, BENE_IFSC, AMOUNT, CREDIT_NARR, PYMT_DATE, MOBILE_NUM, EMAIL_ID, REMARK,
   REF_NO and then one row per payment, nothing else. PAB_VENDOR on every row; FT for an IFSC starting ICIC, NEFT
   otherwise; the debit account, account numbers, IFSC, mobile and date are text (leading zeros kept, left-aligned);
   AMOUNT has 2 decimals and equals the bank advice; names have no dots, hyphens or apostrophes; the date is today
   as DD-MM-YYYY; a mobile saved as "+91 98765-43210" is 9876543210, a short one is blank; REF_NO is the Employee ID.
7. People on salary hold, and anyone whose net is 0, are in neither the bank advice nor the upload. Someone paid a
   "Held salary released" line this month is in both, with it included in the amount.
8. Reports › Payroll › **Bank advice**: the same Bank advice and ICICI bank upload buttons and problems list for the
   chosen month (a note that the month is not approved yet otherwise). **Payroll register**: **Download full register,
   <month>** (works for a Verified month too).
9. Masking: in Access control set Payroll's salary to Masked and Aadhaar and PAN to Hidden, then download the full
   register as Payroll: other people's amounts read "₹ ••••" (totals too), PAN, UAN and ESIC are blank, account
   numbers show only the last 4 digits. Your own row is in full. Put the settings back.
10. Refused: as HRBP (no Export payroll files), or any role with downloads switched off ("Your role cannot download
    files"); the full register of a month never run ("Run the dry run first").

---

## Part M. PF for new joiners, the UAN hold, incentives, variable pay cycle and salary advances (14 Nov)

Sign in as Payroll (EMP-1011), Payroll Manager (EMP-1012) or HR Admin (EMP-1002) unless a step says otherwise.

**M1. PF exemption ("Gross > ₹20,000 and no UAN")**
1. Payroll › Pay schedule and components › **PF for new joiners**: the gross is 20000 and "Hold salary when the UAN is
   missing" is On. Change the gross to 25000 and back: "Saved" each time, and the audit log has "PF settings updated"
   with before and after. An employee or HRBP cannot change it.
2. Add a joiner with monthly gross ₹25,000 who answers **No** to "Do you already have a UAN?" (joining form). Within 5
   minutes (the maintenance sweep), or at the next payroll calculation, the employee's pay section (Employees › the
   person) shows **PF** › **PF exempt**, "Monthly gross ₹25,000 is above ₹20,000 (default ₹25,000 from 19 Nov: use
   ₹27,000) and there is no existing UAN · System".
   Basic is ₹12,500, so a warning reads "PF exempt with Basic ₹12,500 (₹15,000 or less): the law may still require PF".
3. The same with gross ₹18,000 (not exempt: "not above ₹20,000"), with **Yes** to the UAN question (never exempt:
   "Existing PF member"), and someone whose answer is not recorded (not exempt).
4. Payroll run › Before you run payroll: the set-up warnings list "PF exempt with Basic ≤ ₹15,000: check …" with the
   joiner. It does not block the dry run.
5. Dry run: the exempt joiner's row has Employee PF 0 and no employer EPF, EPS, EDLI or admin; ESI is unchanged. After
   approval the ECR and the PF enrolment file leave them out.
6. **Change PF exemption** on the pay section (Payroll or HR Admin only): PF member, reason "Asked to stay in PF":
   "Set by hand" shows; the sweep no longer changes it; the audit log has "PF exemption changed by hand". "Decided by
   the rule" hands it back to the rule. No reason: refused.
7. Someone already paid PF in an earlier month is not changed when their gross or UAN answer changes.

**M1c. PF on the offer (26 Nov 2026)**
1. Raise an offer (Permanent): "Provident Fund › Does the employee want PF?" shows with Select; sending without an
   answer lists "Does the employee want PF? Choose Yes or No". Switch the type to Retainer: the card disappears.
2. No with gross ₹25,000: red note and the error "Gross is up to ₹25,000: an employee who does not want PF must be a
   Retainer. Change the employment type to Retainer."; sending refused.
3. No with gross ₹30,000: breakup Basic ₹30,000, HRA/SA 0, no employee or employer PF; letter says PF not deducted.
   Approve, release, accept, Mark joined: PF card shows PF exempt, "No PF: paid as all Basic", by Offer. Timeline has
   "PF choice on the offer".
4. Yes: normal Basic/HRA/SA with PF.
5. Offer No, then joining details (HR or the joiner's link) "Do you want PF deducted?" prefilled No; change to Yes:
   offer breakup returns to normal; after Mark joined the employee is a PF member (by Joining form); timeline "PF
   choice changed on the joining details/form".

**M1b. PF choice, UAN number and one medical plan (19 Nov 2026)**
1. Payroll › PF for new joiners shows ₹25,000 when the company never saved a value (a saved value is kept).
2. Joining details (HR form and the joiner's link) of a Permanent joiner with gross ₹30,000: Existing UAN available?
   **Yes** shows "UAN number" (required; 11 digits or letters: "UAN: 12 digits"). **No** hides it and shows "Do you
   want PF deducted from your salary?" (Yes / No, required) with the line explaining both answers. With gross
   ₹25,000 (not above) or a Retainer the question never shows.
3. Answer **No**, Send to Payroll, verify. The employee's PF card shows PF exempt, "PF choice: No PF: paid as all
   Basic", all Basic in force from the month, by Joining form. Dry run: Basic = the fixed gross, HRA 0, special
   allowance 0, employee and employer PF 0; ESI, LWF, variable pay as for anyone else.
4. Another joiner answers **Yes**: PF member, normal Basic/HRA/SA split and PF, exactly as before.
5. PF card › **Change PF choice** (Payroll or HR Admin; HRBP cannot): reason required; audit "PF choice changed"; it
   applies from the open payroll month. Refused for someone with a UAN, gross not above the threshold, or (to No)
   someone already paid PF.
6. Offer breakup / offer letter of a candidate who answered No shows the all-Basic version.
7. Marital status **Married**: "Spouse name" appears and is required; Single: no spouse field, nothing required;
   1st/2nd child optional either way. No "Medical insurance nomination" drop-down anywhere; after Send to Payroll the
   offer data holds "Employee + spouse + up to 2 children". Reports › New joiners: spouse and children appear as
   Medical members when there is no medical nomination.
8. Bulk update › Personal details: columns Spouse Name and UAN (12 digits; needs the government ID right); "Existing
   UAN? Yes" with a UAN in the same row is accepted.

**M2. UAN hold**
1. Remove the UAN of a permanent PF member (Statutory IDs). Payroll run › Bank verification: a **PF / UAN: UAN not
   provided** list shows them ("Held at the calculation"); it does not count as pending and the closure closes.
2. Dry run: Salary holds lists the person, reason "UAN not provided", by System, from the month; the row shows On hold
   and the bank advice leaves them out. A PF exempt joiner with no UAN is not held.
3. Approve the month. Next month, enter the UAN and run the dry run: the hold is released by System ("UAN ending … on
   record") and the row has "Held salary released, <month>" with last month's net.
4. Switch "Hold salary when the UAN is missing" Off and run again: nobody is held for a missing UAN (an active UAN hold
   is released at that run). A joiner in the month gets the same rule.

**M3. Incentives (Bulk update › Incentives)**
1. As Payroll or HR Admin: Bulk update › Pay and dues › **Incentives**. Download the template: Employee ID, Month,
   Amount (₹), Remark, with a How to fill sheet. HRBP and employees do not see it.
2. Upload: `EMP-1020 | 2026-11 | 5000 | Sales target Q3` and `EMP-1016 | Nov-2026 | 2,500 | Festival` (with the open
   month): Check file shows "Incentive: ₹5,000 in November 2026 · Sales target Q3". Also try a month already paid
   ("already paid and closed"), amount 0, an unknown ID, and the same row twice ("Appears again"). Apply: 2 rows.
3. Upload the same file again: "Already loaded" for each row; a different remark for the same person and month is a
   new incentive.
4. Close the open month's Reimbursement or Deduction closure (or verify the run) and upload for that month: "locked for
   new pay lines … Use <next month> or later".
5. The incentive shows in Upcoming pay lines, on the payslip as "Incentive", in the payroll sheet's additions, and in
   the full salary register's **Incentive** column (after Variable pay). PF and ESI do not change with it; TDS counts it
   as salary.

**M4. Variable pay on each person's own cycle**
1. Give a permanent employee who joined on 15 May 2026 variable pay of 10%, payout every 3 months. The pay section
   reads "Own cycle from May 2026: next payout July 2026". Payroll pays it in July (May pro rata by days, June, July),
   then October 2026, January 2027.
2. An existing employee who had no variable pay and gets it from the open month: the cycle starts in that month (next
   payout two months later), not from the joining date.

**M5. Salary advances**
1. As a contractual employee, or a permanent one with 2 months' service: My pay › Salary advance explains why it is not
   available ("permanent employees only", "after 3 months' service, from …"); the API refuses the same.
2. As an eligible employee: the form has the amount and "Reason and recovery wish" only. Send ₹9,000 with "please
   deduct in 3 months". The request stores 1 instalment (the full amount in the first month after the payout).
3. As the line manager, open the request: **Recovery** › Recover in **3 monthly instalments**, Approve. The history
   reads "Instalments set by <manager> (Line Manager) when approving: 3 monthly instalments …"; audited.
4. Payroll can still change the recovery at its own step (Change recovery). After final approval: one "Salary
   advance" line and 3 "Advance recovery (n of 3)" lines.
5. Payroll › Pay schedule and components › Salary advances: "Months of service before asking" (default 3) and "Most
   monthly instalments" (default 12) change what employees and approvers see.

## Part N. PAN verification, job roles and skills, cards, optional holidays, import without a salary (14 Nov)

Sign in as HR Admin (EMP-1002) unless a step says otherwise.

1. **PAN verification setting.** Masters › Company › **PAN verification**: Off by default. Choose **Surepass**, give a
   base URL starting `http://`: "must be an https:// address". Give `https://kyc.example.com`, switch On, Save: the
   pill reads "On, not configured". Paste an API key (8+ characters) and Save: the field says "saved, ends ….", the
   key is never shown again and the audit log has "PAN verification settings saved" without the key. As HRBP the
   setting answers 403.
2. **Test provider.** Choose **Test**, On, Save ("On · Test"). Open EMP-1020 (Karan Khan) in Employees with a PAN
   like `ABCPK1234L` (4th letter P, 5th letter K = Khan): **Verify PAN** gives "PAN check: Verified" and a green
   Verified pill next to the masked PAN (hover: name on PAN, date). A PAN with another 5th letter gives "Name
   mismatch"; one whose 4th letter is not P gives "Invalid". Change the PAN (payroll file › Raise a change): the
   pill reads "Not checked" until Verify PAN is pressed again. With Surepass chosen, Verify PAN says "not
   configured" (nothing is sent anywhere).
3. **At joining.** With Test on, open an accepted offer's **Joining details**, enter a PAN and save: the dialog shows
   "PAN check: Verified" (or Name mismatch / Invalid). The joiner's own link does the same when they save their
   PAN. When the Employee ID is created the check moves to the record with the PAN.
4. **Roles and Skills masters.** Masters has **Roles** and **Skills** tabs (add, edit, rename, inactive from a date,
   reactivate) and the Master update workbook has Roles and Skills sheets. Renaming a role renames it on every
   employee and open offer that has it.
5. **Required on the offer.** Raise an offer without Role or Skill: "Role is required", "Skill is required". The
   drop-downs list only active items. After approval the record shows **Job role · skill**. Existing employees
   without a role are not touched.
6. **Employee master update.** Download the template: Role, Skill, Medical and Accommodation sit before Reason for
   change. A row with Role Kitchen, Skill Cooking, Medical 450, Accommodation 1500, a reason and today's date shows
   four changes; apply, and the record shows them (Medical insurance "₹450 a month (set for this employee)"; the CTC
   includes it in place of the structure's rate). "Standard" in Medical goes back to the structure's rule. A role
   that is not active, or a change without a reason and date, is refused on its row. An old file without the four
   columns still works.
7. **ESIC and medical cards.** Bulk update › Employees › **ESIC and medical cards**: only "Choose ZIP file". Upload a
   ZIP with `EMP-1020_ESIC.pdf`, `EMP-1020-medical.png`, `EMP-9999_ESIC.pdf`, `photo.pdf`, a text file renamed
   `EMP-1021_ESIC.pdf` and a 6 MB PDF: the check lists 2 matched, 1 unmatched ("No employee EMP-9999") and invalid
   rows (name, "Not a PDF, JPG or PNG file", "Larger than 5 MB"). Apply: the record's documents list "ESIC card"
   and "Medical card". Upload a new ESIC card for EMP-1020: "replaces the current card, which stays in the history".
   A ZIP whose entries are `../x_ESIC.pdf` or `/etc/x_ESIC.pdf` gets "Unsafe path … refused" rows; nothing is
   written to disk from the ZIP.
8. **Opening optional holiday balances.** Bulk update › Leave and time › **Opening leave balances** (the separate
   optional holiday template is gone since 17 Nov): column **Optional Holiday Balance**. `EMP-1020, , , <today>, 1`
   applies; 40 is refused (0 to 31); a future date is refused; a retainer is refused. A row with a leave type,
   balance and optional holiday balance applies both; a problem in either writes neither. A blank cell keeps that
   balance. The record's Optional holidays line reads "… 1 left this year (opening as at …)".
8a. **Import then later changes.** Import an employee with opening CL/PL, sizes and Attendance code. Inventory ›
   Uniform sizes: not under "No sizes on record". Bulk update › Opening leave balances, Uniform sizes, Personal
   details and Employee master update › Download current data: the imported values are filled in. Upload each file
   unchanged: Check shows no changes and asks for no reason. Each template's help says Import employees is for new
   people once and the others for later changes.
9. **Import employees without a salary.** The template has no Monthly Gross; Role, Skill and Existing UAN? are
   optional columns. An imported person has no gross (record: Monthly gross empty), the dashboard's Details alert
   counts them ("… with PAN, Aadhaar, bank or salary pending"), and Bulk update › Salary breakup sets the gross. An
   old file that still has Monthly Gross is read; each row notes "Monthly Gross ignored: set the salary with Bulk
   update › Salary breakup or Compensation". Existing UAN? Yes without a UAN is refused on its row.
10. **Children and Existing UAN?.** Bulk update › Personal details has 1st Child and 2nd Child after Father or
    Spouse Name, and Existing UAN? last. Blank keeps; Existing UAN? Yes needs a UAN on the record. The record shows
    "Father or spouse · children" and "UAN · existing UAN?"; Employees › Download all details has the children
    (Personal details) and PAN check and Existing UAN? (Statutory IDs). Form setup has Father or spouse name, 1st and
    2nd child name (optional, joiner) next to Marital status; the joiner's "Existing UAN available?" answer is saved
    on the record as Existing UAN?.

## Part P. Access by person and area, policy conflicts, leave by employment type (17 Nov)

Sign in as HR Admin (EMP-1002) unless a step says otherwise.

1. **Give a right.** Access control › Personal rights: pick EMP-1009, Right "Salary and CTC", Give on top of roles,
   View, a reason, Give right. The row shows "Given: View", In force. Sign in as EMP-1009 (Line Manager view): the
   Employee file menu appears. Audit log: "Gave Salary and CTC (View) for Anil Kapoor".
2. **Block a right.** Block "Export employee data" for EMP-1004 (No access, last day next month). As EMP-1004 (HRBP),
   Employees › Download all details is refused. Lift it: the download works again.
3. **HR Admin is locked.** Blocking any right for EMP-1002: "HR Admin always has every right and cannot be blocked".
   Role mapping: switching off the only HR Admin mapping: "This is the last active HR Admin…". Give EMP-1004
   "Users and data access" Edit, then as EMP-1004 (HRBP) try to switch off the HR Admin mapping or map someone as
   HR Admin: "Only an HR Admin can give or remove the HR Admin role".
4. **Scope by cluster, city or state.** Role mapping › Map a role › EMP-1020 as Finance, Data access **By cluster** ›
   Gurugram Cluster 1. Check access › that user: "Employees they can see" = everyone at the Gurugram locations.
   Masters: add a location to Gurugram Cluster 1 and move an employee there: the count goes up by one with no change
   to the mapping. Edit the mapping to **By state** › Haryana: same people. The list reads "Clusters: …" or "States: …".
5. **Who can see what.** Check access › pick a user: below the summary, each right with its level and source pills
   ("HRBP: Edit", "Personal: View until …", "Blocked to No access"). Download gives the same as Excel.
6. **Role change on the employee page.** Employee file › EMP-1020 › Overview › Roles and access: Assign a role
   (Finance, By city › Gurugram), switch it off and on, give or block a right. Each change is in the audit log. A
   role without Role mapping (View) does not see the card; without Edit it cannot change anything.
7. **Policy conflict.** Work policies › Add policy "Hub 5.5 day" › Mappings › Add Location "Gurugram – Sector 29 Hub".
   A dialog lists "Operations – 24x7 rotational is already applied here (through the Front Office department) to N
   people" (and Property leadership through Operations). Cancel: nothing saved. Add again › **Keep their current
   policy**: saved; the hub people in conflict get an Exception mapping on their old policy (note "Kept on …").
   Add the same location to a second new policy › **Replace**: the toast says the location moves; the people in
   conflict now have exceptions on the new policy. Try **Pick people**, tick one person, Save: only they move.
8. **Effective policy.** Employee file › EMP-1009 › Overview › Work policy: the policy, "Exception for this person"
   and the note, plus "Also matched" lines for the mappings that lost.
9. **Overlaps.** Work policies › Coverage › Overlaps lists people matched by mappings of two or more policies, the one
   they get and through what; Download gives the full list.
10. **Company policy for a cluster, city or named people.** Company policies › Add: Chosen groups › Clusters "Gurugram
    Cluster 1": due for Gurugram staff only. Another for City Mumbai; another for named employee EMP-1014 only.
11. **Leave by employment type.** Compensation › Salary structure › Rules by employment type › Leave: set Permanent to
    **Only chosen types** › tick Sick leave. EMP-1020's My time shows only Sick leave; applying Casual leave says
    "Casual leave is not allowed for Permanent employees". Set **No leave**: no leave types, and Bulk update › Opening
    leave balances refuses rows ("Permanent employees have no leave"). Set back to **All leave types**.

## Part Q. Hiring lifecycle: offer versions, Offer ID at acceptance, waiting window, Joining ID, Employee ID (17 Nov)

Sign in as HRBP (EMP-1004) for the candidate steps, HR Admin (EMP-1002) for settings, as in Part B.

1. **Settings.** Onboarding › Form setup › Hiring (HR Admin): offer stays open 7 days, remind 2 days before, waiting
   window 7 days, "HR confirms the close", background check "holds the Employee ID". Change and Save; Cancel undoes.
2. **No Offer ID at selection.** Shortlist, approve, Selected at the interview (Part B). The toast says "HR raises the
   offer"; the candidate still shows SI-…; the trail reads "Offer ID … at acceptance".
3. **Offer v1 and a question.** Raise offer v1, approve it, Release offer. The candidate shows the versions table
   (v1 Sent, answer due date). **Question about the breakup** › "Why is HRA lower?" › Record: status "Question about
   the breakup on v1. Revise the offer"; **Revise: send v2** › change the gross › send › approve › release. v1 reads
   Revised with the question; v2 Sent. **Move the expiry date** needs a reason.
4. **Accepted.** Accepted: toast "… accepted the offer. Offer ID OI-…"; the big ID is now OI-…; v2 Accepted · frozen;
   Revise is gone. The Timeline lists every step with who and when.
5. **Declined / lapsed.** Another candidate: Declined › reason → Closed: Declined. Another: release, then (or wait a
   day after the due date) the hiring sweep closes it as **Offer lapsed**; HR had a bell reminder before.
6. **Joining-day outcomes.** On an accepted candidate: **Joining date moved** (new date, reason) updates the offer;
   **Withdrew before joining** and **Came, declined on the day** close with that kind.
7. **No-show and waiting.** **Did not come** › Start waiting: the pipeline's Waiting column, "No-show: waiting, day 1
   of 7". Log a call, Extend the window (3 days, reason) → day 1 of 10. HR's bell gets "…day N of 10" once a day.
   **Came: mark joined** on today: joined on JI-…, the same offer, documents and joiner link.
8. **Window over.** A no-show whose window has ended: **Close as No-show**. With "Close automatically" on, the sweep
   closes it and tells HR.
9. **Joining ID is a normal employee.** After Mark joined: Employees lists JI-… as Active; a punch for JI-… (machine or
   Realtime with its attendance code) is stored; the payroll month includes them.
10. **Stopped coming / quit.** On a joined (JI) person: **Stopped coming** (first missed day) → Waiting; **Came back**
    → resumes, "N missed days count as absent". **Quit in the first days** (last day, reason, rehire Yes/No) → Closed:
    Early exit; the record is Inactive with that last working day and exit reason Early exit; no separation case.
11. **Background check and the Employee ID.** Upload the report under Background check, Result Discrepancy with a
    note, Save. Joining details complete › Send to Payroll › Payroll verifies: the person stays on JI-… ("waits for a
    clear background check"). Result Clear › Save: toast "Employee ID EMP-… issued". The record, punches, documents,
    payroll rows and requests are now on EMP-…; searching "JI-…" in Employees and opening `/employee-file?id=JI-…`
    still find them; stage Probation.
12. **No Employee ID for a type.** Employees › Employee codes › Contractual › Employee ID "None: keeps this number".
    A Contractual joiner is marked joined on CON-0000N directly and keeps it after approval.
13. **Stage and timeline.** Employee file › Status timeline: Set the stage (Confirmed, reason, date): the timeline
    shows it with the hiring events. Without `emp.status` Edit the selector is not shown.
14. **Drop-off report.** Reports › Workforce › Hiring drop-off: counts by kind for the period, by month, property, role
    or recruiter; Download works with the report download right.
15. **Old candidates.** A candidate selected before this build keeps their OI- number; one who joined before it gets
    the Employee ID the old way (when the documents are complete).

## Part R. Punch machines by cluster: capacity, where records go, the enrol/remove to-do list (17 Nov)

Sign in as HR Admin (EMP-1002). Attendance › Punch machine setup › **Punch machines by cluster**. Use a copy of the
data (never the live database) when the portal account is set.

1. **Import.** Import machines from the portal: the toast says "40 machines read: 40 new, 0 updated"; the Machines
   table lists each with serial, model, In/Out mode, portal branch, vendor Realtime. Import again: "0 new, 40 updated".
   Nothing is pressed on the portal (it is only read).
2. **Map.** For a machine whose branch matches a property, "Suggested: …" with **Use**; "Use the N suggested
   properties" maps them all. Pick a property for the rest from the list; the Cluster column fills in. Not mapped:
   the machine counts nowhere.
3. **Capacity.** Leave "Holds" empty for the default (1,000); type 2 on one machine and press Tab: its cluster's
   capacity drops. Change the default to 800 and Save: unmapped/empty ones follow.
4. **Board.** Each cluster: machines, capacity, people (active, JI- included), free, % used with a bar; Nearly full
   from 90%, Full at 100% (red), "No machines" when none is mapped. Online column: "Not shown by the portal".
5. **Where records go.** On a cluster with machines: own property's machine first; with a small capacity, the rest
   show "(overflow)" on the next machine; too little in all: "No place" and "N without a place" on the board.
6. **Baseline.** On a fresh import (no one recorded): the warning and **Record everyone as enrolled** (confirm). The
   to-do list is then empty.
7. **Full cluster: transfer.** Make a cluster full (step 3). Employees › someone in another cluster › change Location
   to a property of the full cluster: "… is full … Go ahead anyway?" Cancel: nothing changes. OK: saved; Audit log
   "Put into … although its punch machines are full". The to-do list shows **Move**: "Enrol in cluster B (…), remove
   from cluster A".
8. **Inside a cluster.** Change someone's location to another property of the same cluster (even when full): saved
   without a question; no to-do.
9. **Joiner and exit.** Mark a candidate joined at a property of a full cluster: the same question; OK → JI-… and an
   **Enrol** task "Enrol in cluster X (machine …)". Mark someone inactive (exit): a **Remove** task.
10. **Tick and export.** Tick tasks › Tick done: they move to "Done in the last 30 days" (Ticked by …). Download Excel:
    one row per open task (task, Employee ID, name, code, property, clusters, machine).
11. **Portal check.** Give a joiner their Attendance code and enrol them in the portal; "Match portal codes" › Fetch
    the list again: the task is ticked ("Seen in the portal"); a task without a code reads "No Attendance code: tick
    it by hand".
12. **Rights.** An Employee or a role without Punch machine setup cannot open the board or change anything (403); View
    only sees no buttons.

## Part S. Staff accommodation: units, leases, beds and the cost split (17 Nov; not part of payroll)

Sign in as HR Admin (EMP-1002), on a copy of the data. Left menu › **Accommodation**.

1. **Unit.** Units and leases › Add unit: name, Flat, a cluster, For Any, 2 beds; add two properties it serves (the
   first shows "Home property"; ↑↓ reorder). Save without a property: refused. Same name again: refused.
2. **Lease.** Open the unit › Add lease: start 1 Jan, rent 12,000, deposit 24,000, notice 30 days. Add a second lease
   overlapping it: refused ("one lease at a time"). Upload an agreement on the lease: it shows as a chip and opens.
   A lease with 5% from a date: "next on …" shows; the rent changes from that date.
3. **Check-in.** Check-in and check-out › Assign accommodation › pick an employee of that cluster: the unit is
   listed ("Serves … first"), with free beds. Check in A from the 1st of last month (bed 1), B from the 16th of this
   month (bed 2). A third person: "no free bed". A in another unit at the same time: "already stays … check them out
   there first". A Male-only unit for a female employee: refused.
4. **Split, equal per person (default).** Month cost split, this month (30 days): A 6,000, B 6,000 (both stayed in
   the month, however many days), no empty-unit cost. Enter bills on the unit (electricity 1,800, water 300, Wi-Fi
   900): A 7,500, B 7,500. With four people in a 12,000 unit: 3,000 each. A unit with nobody all month: its whole
   cost is empty-unit cost under its home property.
5. **Split by days stayed.** Settings › tick **Split by days stayed** › Save. The split page now says "Split by days
   stayed": A 11,250, B 3,750 (A alone 15 days, then shared); a day with nobody is empty-unit cost. Untick it and
   save: back to 7,500 each.
6. **Check-out.** Check A out on the 11th of next month: bed 1 can be given to someone from the 11th itself. Reduce
   the unit to 1 bed: refused while B is on bed 2. Mark the unit inactive: refused while people stay.
7. **Prompts.** Mark B inactive with a last working day (Separation or a copy): B appears under "To check out"
   (Exit, suggested date = the day after the last working day); a bell message arrives within 5 minutes. Move someone
   to a property of another cluster: "Transfer".
8. **Not payroll.** Payroll › open month: checking someone in, changing a rent or entering bills does not mark the run
   for recalculation. Dry run: the person's row, payslip preview, payroll sheet and full salary register are exactly
   as before the check-in: no accommodation earning or recovery line, no extra column, the same PF, ESI, LWF, TDS,
   net pay and CTC. The person's manual **Accommodation** deduction (Salary breakup "Accommodation Deduction", if
   set) still shows on the payslip and in "Meal and accommodation" on the sheet as before. The employee file's
   Accommodation tab shows the stays, with no salary figure.
9. **Board.** Occupancy board: beds, filled, free by gender per cluster, month cost, allocated, empty-unit cost,
    leases ending in 60 days; Download Excel.
10. **Rights.** An Employee (or a role without the Accommodation rights) does not see the page and gets 403 from
    `/api/hst/*`; View-only sees no buttons. Every change is in the Audit log.

## Part T. Pay structure designer: components, fixed and variable pay (17 Nov)

Work on a copy of the data. Sign in as HR Admin (EMP-1002) for Compensation and Payroll (EMP-1011) for the run.

1. **Nothing changed.** Before anything else, on the copy: `hrms pay-compare -month <last month>,<open month>`.
   It prints "0 with differences" for each month. Payroll › open month › Dry run: every net, PF, ESI and CTC is as it
   was on the frozen binary.
2. **Converted structures.** Compensation › **Pay components**: the library lists Basic, Dearness allowance, HRA,
   Special allowance, Performance pay. Click Version 1: the note says it uses percentage rates, and its components
   (Basic + DA % of gross, HRA % of Basic, Special allowance balancing, Variable pay as a target after migration 017).
   Change Version 1's Basic % in Salary structure and back: the components follow.
3. **Library.** Add component › Conveyance, Fixed earning, Fixed ₹ 1,600, maximum 1,600, ESI wage, not taxable,
   on payslip › Save. The same code again: refused.
4. **New structure.** New structure from components › name "Sales 2027"; it starts with Basic, HRA, Special
   allowance. Set HRA to 40% of Basic; add Conveyance, Performance pay (variable, monthly) and a deduction (add a
   library component "Canteen", Fixed deduction, ₹300). Create. Two balancing fixed earnings, or two components that
   are a % of each other: refused with the reason. The wage check shows at least 50% (Basic 50% of fixed).
5. **Preview by CTC.** Try pay: CTC a month 35,000, variable ₹15,000: fixed 20,000; Basic 10,000, HRA 4,000,
   Conveyance 1,600, Special allowance 4,400; Performance pay target 15,000 a month; Canteen 300 under deductions.
   The warning "Basic + DA is 28.57% of total pay" shows (a warning only). Variable 30% of the total by gross 21,000:
   variable 9,000.
6. **Map and target.** Payroll › Structure mapping: move one employee to Sales 2027 from the 1st of the open month.
   Payroll › **Variable pay**: the person is listed, Performance pay due, "Not entered". Type 15000 under Target a
   month (saved from the open month).
7. **Not entered.** Payroll run › Dry run. The row has no variable pay; the checks show "Variable pay due but not
   entered" with the name. The payslip preview lists Basic, HRA, Conveyance, Special allowance and Canteen by name.
8. **Entered.** Variable pay › Amount to pay 12,000 (Tab away). Bulk update › **Salary breakup** (Variable payouts is
   no longer in the picker) › Download current data: the person's row has Payout Month (the open month), Payout
   Component "Performance pay" and Variable Payout (₹) 12000; change to 13000, Check file, Apply. One row with a
   Meal Deduction change (reason and Effective From) and a payout applies both. Payroll (no salary right) can upload
   a file with only Employee ID and the payout columns; a salary value on such a row is refused. Dry run again: variable paid
   13,000 on the row and payslip ("Performance pay"), the check is gone. Full salary register: columns
   "Component: Conveyance", "Variable: Performance pay" and "Canteen". A month the component is not due in (a
   quarterly one in November): refused.
9. **Offer.** New offer on Sales 2027: the Compensation card has "Variable pay given as" (% of gross / ₹ a month)
   and "Variable pay target" in the row with Monthly gross, Salary structure version and Variable pay payout (a %
   shows its ₹ amount under it); the breakup on the right only displays. "Enter pay by CTC" (under the Compensation
   card) › 35,000 with ₹15,000 variable › Use this: gross 20,000 and
   Variable target 15,000; the breakup lists the components. An offer template with `{{breakup}}` prints one line per
   component.
10. **Bulk salary.** Bulk update › Salary breakup › Download: the person has Total Pay 35,000 and Variable Target
    15,000. Set Total Pay 40,000 with a reason and Effective From in the open month: gross 25,000. Monthly Gross and
    Total Pay that disagree: refused. A Variable Target for someone on a percentage structure: refused.
11. **Full and final.** Give the person a last working day: Payroll › Full and final › open: "Performance pay,
    current cycle: not entered" with an amount box; enter 4,000: the line becomes an earning of 4,000.
12. **Rights.** An Employee gets 403 from `/api/comp/components` and `/api/pay/variable`; HRBP cannot enter variable
    amounts. Every change is in the Audit log.
13. **Version 1 on targets (migration 017).** On a copy, before the new binary: `hrms pay-compare -month <last>,<open>
    -dump before.json`; run `hrms migrate` (applies `017_variable_target`), then `hrms pay-compare -month <last>,<open>
    -against before.json`: "0 with differences" unless someone with a variable % is paid in those months (their
    variable is then no longer paid automatically; nothing else differs). Compensation › Salary structure › Version 1:
    the note says variable is a target per person. EMP-1054's file: "Variable pay target 11.11% of fixed gross
    (₹2,999.70 a month)"; Employee history shows the move by Migration 017.
14. **% or ₹ everywhere.** Pay options of an employee: choose "% of gross", 10: the hint shows the ₹ amount on the
    gross; Save: the target applies from the open month. Choose "₹ a month", 4000, Save. Salary calculator and
    Compensation › Try an offer: both modes change the breakup's variable. New offer: in the Compensation card the target
    fields (Variable pay given as + Variable pay target) where "Variable pay, % of gross" was. Approve and join:
    the employee has the same target and mode. Bulk update › Salary breakup: download shows Variable Target and
    Variable Target In; set 12 and %, check and apply: the target is 12% of gross. Variable Pay % on such a row: refused.
15. **Suggestion.** Payroll › Variable pay: a due row's box shows "Suggest …" = target × months in the cycle and a
    "Use ₹…" button; click it: saved and the payroll check clears. Leave another blank: not paid, check listed.

## Part U. Cost and budget console: cost by place, budgets, frozen months, headcount plan (17 Nov)

Work on a copy of the data. Sign in as HR Admin (EMP-1002).

1. **Menu and rights.** Cost and budget is in the left menu for HR Admin. An Employee and a Property Manager without
   `cost.view` get no menu item and 403 from `/api/cost`. Access control lists "Cost and budget" (View) and "Cost and
   budget: budgets" (Edit).
2. **Last published month.** Pick the last published month: the pill says "Actual · frozen on <date>" (the sweep
   freezes months published before this build within 5 minutes of the start). Expand Company › a state › a city › a
   cluster › a property. Each row: people, salary paid, contributions, accommodation, total, per head. Hover the
   contributions for PF, admin/EDLI, ESI, LWF, gratuity, bonus, medical. The company's salary paid + contributions
   equals the payroll sheet's total earnings + employer cost for that month, less the meal and accommodation
   deductions (the footnote shows the amount left out).
3. **Top-ups are not cost.** Someone with a ₹2,000 Meal Deduction in Salary breakup: People (on their property) shows
   salary paid ₹2,000 below their earnings (less bonus), with the asterisk tooltip.
4. **Accommodation.** With a unit and a stay (Part S) in the month, the person's line has their share and the unit's
   home property shows the empty-unit cost under Accommodation (hover: shares · empty units).
5. **Budgets and over budget.** On the open month, a property › Budget: monthly cost 10,000 › Save: the row shows
   difference negative, % used above 100 and "Over" in red. Set it high again: the flag goes. A cluster › Budget with
   both boxes empty uses the total of its properties ("total of properties"); set 5,000: the cluster uses 5,000.
   Compensation › Budgets shows the same property figure. Audit log: "Budget changed (cost console)".
6. **Projected.** The open month shows "Projected" and the warning note. Change someone's gross from the open month:
   the projection follows. The figures change until the run is approved.
7. **Frozen stays frozen.** On the published month: change a budget (from the open month), add an accommodation bill
   for that month, correct a pay line: the published month's figures and budgets do not change.
8. **Trend and Excel.** Trend on a cluster: six months, bars and table (Frozen / Actual / Projected per month).
   Download Excel: one row per place with every column, indented by level.
9. **Headcount plan.** Tab Headcount plan: per cluster and property, seats, in post, offers in flight, in approval,
   free. Accept an offer (not joined): offers in flight +1. Raise an offer for a property with no free seat: the
   offer shows "location headcount budget" over, the same count as the plan. A cluster with its own seats set: an
   offer past it shows "cluster headcount budget".
10. **Bulk.** Bulk update › Budgets › Download with data: Cluster rows are listed. Set a cluster's Monthly Payroll
    Budget to 0: the cluster goes back to the total of its properties.
11. **Scope.** Map a Property Manager to one property and give the role `cost.view` View: they see only that property
    (no "Not tied to a property" row), no People button, and cannot change budgets.

## Part V. Automatic sending to the Realtime portal (19 Nov; replaces the 17 Nov Send button)

Work on a copy of the data. Sign in as HR Admin (EMP-1002). Never test against the live portal: the first live write
is a real joiner (README, "Sending employees to the Realtime portal").

1. **Switched off (trial build with `Spec.Send = false` only; sending is on since 25 Nov).** Mark a candidate joined:
   their Realtime portal card says "Waiting to send" and "Sending is switched off"; Attendance › Punch machine setup ›
   Realtime portal lists them under Waiting. There is no Send button anywhere.
2. **Rights.** An Employee sees no card; a role with `att.portal` View sees the card and the list, without Retry.
3. **Mapping hold (fake or trial portal only).** Leave the joiner's department unmapped and mark them joined: the
   card shows "Held: mapping needed" with "Map the department … first"; you get a bell. Map it: within a minute the
   card shows "On portal" with the new code, which is now the Attendance code; the audit log has the fields and reply.
4. **Changes and exits.** Change that person's designation: within a minute the portal is updated ("Writes" lists an
   update). Make them inactive: they are marked left with the leaving date.
5. **Failure.** Make the (fake) portal refuse: the card shows "Failed" and the portal's words, you get a bell, and
   nothing is sent again by itself (wait two minutes; "Writes" still lists one). Press **Retry**: one more write.
6. **Backlog.** With items Waiting (left from before 25 Nov, or a build with sending off), press **Send waiting (n)**
   once; each goes out.
7. **Punch machine on a create (25 Nov; fake or trial portal only).** Map a machine (serial as on the portal's
   "Select Devices" list, e.g. RSS20230761581) to the joiner's property and mark them joined: the card's "Writes" line
   reads "create · code … · saved · punch machine RSS20230761581 / Gulmohar Marg"; on the portal the employee's Select
   Devices shows that machine. With no machine mapped to the cluster, or the cluster full (capacity used up), or a
   serial the portal's list does not have: the create still goes, without a device, and the line shows "No punch
   machine chosen: …; HR adds them to a machine on the portal". Change their designation: the update leaves the
   device as it is. No device button (Connect, Upload Name, Enable/Disable/Delete User) is pressed.

## Part Z. Company accommodation from the offer (19 Nov)

1. **Offer form.** Raise an offer: the card **Company accommodation** has No / Yes. Yes without a cluster: "Choose the
   cluster…". The cluster list shows only clusters with accommodation units, with free beds.
- [ ] Release the offer: the release asks the same question (prefilled); the offer shows "Company accommodation:
      Yes, in <cluster>"; the salary breakup does not change.
2. **Mark joined.** The candidate shows "Company accommodation: Yes, in <cluster>". Mark joined: Accommodation ›
   Check-in and check-out shows a stay from the joining date, note "Allocated automatically from the offer"; the
   employee timeline has the same line; nothing was added by hand.
3. **No bed.** Fill the cluster's beds and mark another tagged joiner joined: no stay; Accommodation › **Waiting for a
   bed** lists them; HR gets a bell. Check someone out of that cluster: within 5 minutes the waiting joiner has the
   bed (check-in that day) and leaves the list. "No longer needed" (with a reason) removes someone from the list.
4. **Payroll.** Payslip, register and CTC of the joiner are unchanged by the accommodation.

## Part AA. Skip approval on onboarding raises (19 Nov)

1. With the Shortlist, Joining (offer) or Payroll verification flow's "Approval hierarchy required" unticked, as HR
   Admin: the shortlist, offer and "Send to Payroll" show **Skip approval** next to the send button; it asks a reason
   and the request is approved at once (history "Approval skipped by …"). With the box ticked, or as a non-HR role,
   the button is not shown and the server refuses `?skipApproval=`.

## Part W. New joiners for insurer, EPF and ESIC; which report each role sees (18 Nov)

1. **Joiners export.** As HR Admin, Reports › Workforce › New joiners: pick **Joined in month** = this month (From and
   To change to the month). Columns include father or spouse name, date of birth, gender, mobile, email, Aadhaar,
   PAN, UAN, Existing UAN?, ESIC number, bank name, account, IFSC, monthly gross, and PF nominee 1… (name, relation,
   date of birth, share %) and Medical member 1… (name, relation, date of birth) for people with those nominations.
- [ ] **Download Excel** gives one row per person with the same columns; Download CSV still works.
- [ ] Set HRBP's Aadhaar and PAN mode to Masked (Roles and rights › Masking and downloads). As HRBP the Aadhaar, PAN,
      UAN and ESIC columns show only the last four characters (screen and file); your own row stays in full.
- [ ] As a role without the bank right, bank account and IFSC are blank; without the salary right there is no gross
      column; nominee names and dates of birth follow the medical mode.
2. **Reports by role.** As HR Admin, Access control › Roles and rights › pick HRBP › **Reports this role sees**.
- [ ] Reports are grouped as on the Reports page, all ticked; Payroll reports are marked "Rights do not allow".
- [ ] Untick Employee master list. As HRBP it is gone from Reports; opening `/api/reports/emp_master` or its download
      gives "This report is not given to your role". Tick it again: it is back.
- [ ] Untick all in a group, then Tick all: every report of the group comes back.
- [ ] HR Admin: the list is greyed with "HR Admin always sees every report".
- [ ] Add a role copied from HRBP while a report is unticked: the new role has it unticked too.
- [ ] Activity log shows "Reports of role “HRBP” changed" with before and after.

## Part X. Role and Skill required, hiring IDs for every employee, offer letter at joining (18 Nov)

1. **Role and Skill.** Bulk update › Import employees: download the template; Role and Skill are required columns.
- [ ] A row without Role or Skill: Check says "Role is required" / "Skill is required". A file without the two columns is refused.
- [ ] Onboarding › raise an offer with Role or Skill blank: Send is refused with "Role is required" / "Skill is required".
- [ ] Clear an employee's Role and Skill (test copy only), then Employee master update with a new Department and
      Designation and blank Role/Skill: the row says "Role and Skill are required when the job changes". Give them: it applies.
- [ ] An employee without Role/Skill whose row changes only the shift or manager: applies as before.
2. **Hiring IDs.** After migration 018 (or an import):
- [ ] Employee file header shows SI-… · OI-… · JI-… with one number; Status timeline shows the same and an entry "Hiring IDs: …".
- [ ] A person hired through Onboarding shows their candidate's number. An imported employee shows the next number of
      the candidate series; the next shortlist takes the number after it. The Employee ID is unchanged.
3. **Offer letter at joining.** Onboarding › Form setup › Hiring › Release the offer letter at: tick Housekeeping (or an employment type), Save.
- [ ] Raise and approve an offer for that department: the offer screen says "Offer letter released at joining" and
      shows "The offer letter is held until joining" instead of the letter. Release offer and record Accepted work as usual.
- [ ] Mark joined: the offer screen now shows the letter and "Released at joining on …"; Activity log has "Offer letter
      released at joining for …"; the candidate's timeline has the same.
- [ ] Another department: the letter shows from approval as before.

## Part Y. Variable pay from joining, cost budgets (CTC), retainer fee (18 Nov)

1. **Variable cycle from joining.** On a test copy put someone who joined in May on a structure made from components
   with a quarterly variable component and a target (Payroll › Variable pay).
- [ ] Payroll › Variable pay for July: the component is Due ("From May …"); for August: Not due, "next Oct …".
      Half-yearly: due October and April; yearly: April the next year; monthly: every month.
- [ ] Enter an amount in a month that is not due (screen or Bulk update › Salary breakup payout columns): refused,
      the message names the cycle "counted from May …" and the next payout.
- [ ] Employee file › pay section shows "Own cycle from May …: next payout …".
2. **No target, no payout.** Clear the person's target (or set 0) in a due month.
- [ ] The row shows "No target: set one first" and is not counted in "due"; entering an amount says "… has no variable
      target …"; the same row in the bulk file is refused on Check. Entering 0 is allowed.
- [ ] Full and final: an amount for the current cycle can still be entered.
3. **Budgets as total cost.** Bulk update › Budgets: the template's last column is "Monthly Cost Budget (CTC)".
- [ ] An old file with "Monthly Payroll Budget" still checks and applies.
- [ ] Compensation › Budgets: headers "Monthly cost budget, CTC (₹)" and "Current monthly cost (CTC)"; the cost is
      higher than the gross total (employer PF/ESI included) and matches the property's Salary + Company contributions
      on Cost and budget for the open month.
- [ ] Set a location's cost budget just above its current cost plus a new hire's gross, then raise an offer there:
      the offer shows over "location cost budget (CTC)" (the hire's employer PF/ESI tip it over).
4. **Retainer fee.** On a test copy, a Retainer employee:
- [ ] Salary calculator / Compensation preview with type Retainer: one "Retainer fee" line, no Basic/HRA/Special
      allowance, no PF/ESI; TDS as before.
- [ ] Offer for a Retainer: breakup and letter `{{breakup}}` say "Retainer fee: ₹… a month (TDS 1% of gross)" (25 Nov; was "section 194J").
- [ ] Payroll dry run: the payroll sheet, register and full salary register have a "Retainer fee" column (only when a
      retainer is paid); Basic/HRA/Special allowance are 0 for the retainer; the net is the same as before.
- [ ] Employee file › Salary: "Retainer fee" line, TDS (194J), net pay unchanged.

## Part AB. Half-day leave, short leave, Reimbursement in fixed pay (20 Nov)

1. **Half day.** Policies › a leave plan: "Half-day leave allowed" Yes.
- [ ] My leave › Apply: tick Half day, choose "Second half on leave": the To date goes, the preview says "Half a day";
      after sending, the request shows "(second half)" and the balance shows 0.5 waiting.
- [ ] Approvals › New leave request as HR for someone else: "Half day" list (first / second half) does the same.
- [ ] Approve a half-day Casual leave on a past day with punches 14:00–18:00 (G shift): attendance "Casual leave (half
      day, …) + Present", no LOP. With 14:00–15:30 (under the half-day hours): "+ Absent", 0.5 LOP.
- [ ] A half-day Loss of pay with the other half worked: 0.5 LOP in the month and in the payroll dry run.
- [ ] Bulk update › Opening leave balances with 4.5: accepted and shown as 4.5.
2. **Short leave.** Policies › leave plan › Short leave: allowed, 2 a month, 2 hours (defaults).
- [ ] My leave › Apply › Leave type "Short leave": date, from-time, to-time; 09:00–11:30 is refused ("at most 2
      hours"); 09:00–11:00 goes to the leave approvers (same route as casual leave); no balance changes.
- [ ] A second one in the month is accepted, a third is refused ("2 times a month"). On a weekly off: refused.
- [ ] HR raises one for an employee with "Skip approval" and a reason: approved at once, history says skipped.
- [ ] After approval, punches 11:00–18:00 on a 09:00–18:00 shift: the day is Present, note "Short leave 2h", no late
      mark, no deviation; without the short leave the same punches are a half day / late.
- [ ] A short leave 16:00–18:00 and an out punch at 16:00: no early exit.
- [ ] Reports › Attendance and roster › Short leave lists it with the hours and status. My leave shows date and times.
3. **Reimbursement.** (Superseded 25 Nov: the reimbursement is now outside the gross; test it with Part AK.) On a test copy, Employees › someone permanent on Version 1, gross ₹30,000 › Edit pay options.
- [ ] Reimbursement 3,000: the preview shows Basic 15,000, HRA 7,500, Reimbursement 3,000, Special allowance 4,500,
      gross 30,000; employee PF on Basic only. Save, then the pay section shows "₹3,000 a month, inside the gross".
- [ ] Reimbursement 7,600 is refused ("more than what is left after Basic and HRA (₹7500 …)").
- [ ] Payroll dry run: payslip has a "Reimbursement" line, the payroll sheet a "Reimbursement" column, the full
      salary register "Reimbursement (fixed pay)"; net pay and gross unchanged, Special allowance lower by the amount.
- [ ] Bulk update › Salary breakup: download shows the Reimbursement (₹) column; change it and upload with a reason and
      Effective From; a value above the room is refused on Check.
- [ ] Compensation › Pay components: "Reimbursement" is in the library, method "Amount set on each employee", flags
      taxable only; every structure lists it before Special allowance.
- [ ] Payroll › Salary calculator: "Reimbursement, ₹ a month" input changes the breakup the same way.
- [ ] Offer form › Compensation: Reimbursement 3,000 on gross 26,000 shows in the offer breakup and letter; 7,000 is
      refused before raising. After acceptance and Mark joined, the employee's pay section shows ₹3,000.

## Part AC. Hiring IDs from the Employee code; blank nomination forms (24 Nov)

1. **Hiring IDs** (on a test copy; migration 025 runs at start).
- [ ] An existing imported employee EMP-1020: the employee file header shows SI-1020 · OI-1020 · JI-1020; Status and
      stage shows "from the Employee code"; the timeline has one "Hiring IDs: SI-1020, OI-1020, JI-1020" entry whose
      reason names the old SI-00xx IDs. A person hired through Onboarding still shows the candidate's SI-00xx.
- [ ] Import employees with codes ZZ-000004 and 7350: SI-000004 / OI-000004 / JI-000004 and SI-7350.
- [ ] Import a code whose number a candidate or another employee has (e.g. CON-1020 next to EMP-1020): a
      candidate-series number (SI-0xxx), and the timeline reason says "already used by employee EMP-1020".
- [ ] Onboarding › new shortlist after the import: the number is above the highest imported number (SI-7351 here).
2. **Blank nomination form.** Policies › Nominations › Nomination policy › Edit "Provident fund (EPF) nomination".
- [ ] Documents needed to enrol has a Blank form column; on "Signed nomination form" Attach a PDF: the file name shows;
      Replace with a .docx: the new name shows; an .exe is refused. A row added but not saved says "Save first".
- [ ] A permanent employee, My pay › Nominations › Add nominees: "Download blank form" next to Signed nomination form
      downloads the .docx under its name; they fill it and upload it in the same field as before.
- [ ] A contractual employee (the nomination does not apply) cannot download it (the file link gives 403).
- [ ] Remove: the link goes from the employee's form; `audit_log` has "Blank form attached / replaced / removed …".
- [ ] With Google Drive connected: the form appears in the Company folder as "Blank form Signed nomination form
      (Provident fund (EPF) nomination).docx"; after Remove it is renamed "(replaced …)".

## Part AD. Work policies: a department at a place, hours and minutes, holiday calendars (24 Nov)

1. **Department + place.** Work policies › Operations – 24x7 rotational › Applies to › Add mapping › Department +
   Location › Front Office at Gurugram – Sector 29 Hub › Add.
- [ ] Saved with no conflict dialog; the table reads "Department + Location · Front Office at Gurugram – Sector 29
      Hub" with the hub's Front Office headcount; the note lists the new order (Individual > Department + Location >
      … > Employment type). EMP-1009's employee file › Work policy: through "the Front Office department at location
      Gurugram – Sector 29 Hub". Coverage gains a "Department + Location" column.
- [ ] Add policy "FO cluster" › Department + Cluster › Front Office at the hub's cluster: the conflict dialog lists the
      hub people "through the Front Office department at location …"; Cancel saves nothing.
- [ ] Switch the combined mapping off: EMP-1009 is back "through the Front Office department"; the row stays (off).
- [ ] A person mapped by name stays on that policy whatever combined mapping matches them.
- [ ] **Already applied.** Front Office is on Operations – 24x7 rotational. Open Operations – 6 day › Applies to ›
      Add Department › Front Office: "Already applied to Operations – 24x7 rotational. Switch it off there or edit that
      mapping." and nothing is saved. The same for a Department + Location pair that another policy has, and for
      switching on a switched-off row whose twin is on elsewhere. Adding a mapping the policy already has: "Already
      applied to this policy". Department on one policy and Department + Location on another is allowed (the conflict
      dialog asks about the people).
- [ ] Add mapping opens on **Department + Location** (department, then location); the other levels are in the list.
- [ ] **Edit.** Every switched-on row has Edit: change the department or location (or the value) and Save; or pick
      another Work policy: "Move … from A to B?" with the people who move, Confirm move. A value another policy has is
      refused. The old row stays, switched off; History on both policies shows "Mapping moved"; the Activity log has
      "Work policy mapping edited".
- [ ] **Bulk upload** (button on Applies to opens a dialog on the same page, or Bulk update › Leave and time › Work policy mapping): download the
      current data (the pairs on now). A file with a pair on another policy and Move blank: "Already applied to …";
      Move = Yes: moved, with "N people's work policy changes"; Active = No: switched off; a misspelt policy, department
      or location: "… not found"; the same pair twice: the later row "Appears again (first on line …)".
2. **Hours and minutes.** Policy › Working time › Working hours.
- [ ] Full-day hours, Half-day hours and Maximum spread over show two boxes, h and min. Set 8 h 30 min, save, reopen:
      8 h 30 min (Summary: "Full day 8 h 30 min"); History shows the change in h and min. A stored 9.75 shows 9 h 45 min.
- [ ] Half day above full day, or full day above the spread over, is refused with a message.
3. **Holiday calendars.** Policies › Holiday calendars.
- [ ] Add calendar "QA 2027": the Year box fills with 2027 and "Start empty" is the default; it is added with no
      holidays. Add another with "Copy from NCR 2026": the holidays are copied.
- [ ] Open a calendar › change a holiday's date, name or type › Save on that row: saved; Activity log has it. A date in
      another year ("not in 2026"), or a date that is already a holiday, is refused. With a payroll month approved or
      published, moving a holiday into or out of it, adding one there or switching one there off is refused
      ("… is already paid").
- [ ] The Active switch on NCR 2026 (used by policies) is refused, naming the policies. On an unused calendar it goes
      off; that calendar no longer appears in a policy's Holidays tab, and saving a policy with it is refused.
- [ ] Rename a calendar in its dialog (Save name); a name another calendar has is refused.

## Part AE. Leave earned by working days: paid days, credited the day after (24 Nov)

Policies › Leave plans › Standard leave plan › tab **Earn by working days**. Use a copy of the data.
- [ ] No rule at first; Add rule shows an empty **Paid days** box and one empty row (Leave type, Days earned): nothing
      is filled in. The note says working days here are "paid days (present, half day 0.5, paid leave, weekly off and
      holiday; loss of pay days do not count)", credited "from the day after the Nth paid day". Type Paid days 7,
      Casual leave 1; Add leave type, Privilege leave 1; Save changes: "Saved". History shows "Earn by working days:
      — → every 7 paid days: Casual leave 1, Privilege leave 1".
- [ ] Add a second rule with Casual leave again: Save says "Casual leave is already in the rule for every 7 paid
      days. A leave type can be in only one rule". A rule without paid days, without a leave type or with days earned
      0.3 is refused with its reason.
- [ ] Leave types tab: Casual leave's Credit reads "By working days (see Earn by working days)"; Days a year says it is
      an optional cap (blank or 0: no cap); the card text names paid days and "credited the day after the Nth paid
      day". The plans list shows "… Casual, Privilege by working days".
- [ ] The policy's Leave section and the Leave simulator say "CL earned by working days, 1 day for every 7 paid
      days…". The simulator's Paid days in a month is empty; type 31 and pick a joining date of 15 Jan: CL and PL get
      1 day on 22 Jan, 29 Jan, 5 Feb, … (the day after every 7th day), shown in "Credited on", starting again with the
      leave year. With Paid days 30 on the rule instead, the first credit is on 14 Feb (not 1 Mar).
- [ ] An employee of that plan, My leave: Casual leave shows "Earned by working days, 1 day for every 7 paid days,
      credited the day after the 7th paid day: X earned from N paid days up to <yesterday>, last credited on …; …
      more paid days to the next block". Open it: paid days by month with each credit's date. Check a past month
      against My attendance: every day of the month less its loss of pay (Present, Late, paid leave, Week off and
      Holiday 1, Half day 0.5, Absent / loss of pay leave 0); payroll's paid days for that month are the same less
      any late-mark half days. No upfront 8 days any more.
- [ ] A No punch day of this week (inside the regularization window) counts 1 today. Once the window has passed and
      it is loss of pay, the count drops by 1 (and a block, if that one completed it).
- [ ] Regularize an absence of last month to present and approve it: the paid days rise by 1 (and a block when it
      completes one, credited the day after the day that completed it).
- [ ] Apply for Casual leave from a date next month: the balance shown is the balance on that date, counting the paid
      days over by today only.
- [ ] Mark a test employee inactive with a last working day of last week: their Casual leave counts paid days up to
      that day only, nothing after. A Pre-boarding joiner's Casual leave stays 0 until they are active.
- [ ] Days a year 2 on Casual leave: the credit stops at 2 ("capped at 2 for the leave year").
- [ ] Remove the rule and save: Casual leave is back to its Credit (Monthly / Upfront) and the balances are what they
      were before.

## Part AF. Shifts: punch rules in the work policy, split shifts; no night shift card (24 Nov)

Working time and Attendance tabs of a work policy, Masters › Shifts. Use a copy of the data.
- [ ] Working time has no **Night shift** card. Attendance › Punching still has "Night shift counts on". The policy
      History shows no lines about night hours, the allowance or women at night, even old ones.
- [ ] A payroll dry run of the open month for someone who worked nights: no "Night shift allowance" line; every net
      pay is as before. The full register has no Night shift allowance column.
- [ ] Attendance › Punching has **Maximum early in punch allowed** and **Maximum late out punch allowed**, each as
      h + min, both 0 h 0 min. 12 h 30 min is refused ("… must be from 0 to 12 h"); 12 h saves.
- [ ] Roster someone on a 09:00–18:00 shift for a past day and send punches 07:30, 08:10, 18:40 and 20:30 (G7). With
      both limits 0 the day is 07:30–20:30. Set early 1 h 0 min, late 2 h 0 min, Applies from that day, Save: the day
      becomes 08:10–18:40 (07:30 and 20:30 are outside the window). Set both back to 0: 07:30–20:30 again. A day in a
      month locked by payroll does not change.
- [ ] "A day with a missing punch counts as" lists **Half day until regularized**, Absent, Present until regularized.
      With it, a day with one punch is half a day of loss of pay; an approved regularization clears it.
- [ ] Masters › Shifts › Set shift has no Punch rules and no Single punch rules card. The Shift card has **Second
      part start** and **Second part end** (the Attendance rules card went later the same day: Part AJ). A shift with old punch rules saved
      before today opens and saves, without them; its version history does not show them.
- [ ] Add shift SP: 06:00 to 10:00, second part 15:00 to 19:00, Save. The list shows "06:00–10:00 + 15:00–19:00"; so
      do Set shift ("In effect today"), its version history, the shift pickers (Hire › offer, Change of shift
      request) and the roster (legend under the grid, the cell's tooltip).
- [ ] Refused with the reason: a second part start without an end; a second part starting at 09:00 or at 10:00
      ("must start after the first part ends (10:00)"); 15:00–15:00; a second part on a shift that ends the next day
      ("A split shift must start and end on the same day").
- [ ] Roster EMP-1020 on SP for past days and regularize (or punch) these, on an 8 h / 4 h policy:
      06:00–19:00 → Present (8 h: the 5 h gap does not count); 06:00–10:00 → Half day; 15:00–19:00 → Half day;
      06:00–13:00 → Half day (leaving in the gap); 06:30–19:00 → Half day (7 h 30 min, late from 06:00);
      06:30–19:30 → Late.
- [ ] With "If out punch is missing" = Auto-close at shift end, an in at 06:00 and no out closes at 19:00 (8 h).
- [ ] Master update workbook › Shifts has "Second part start" and "Second part end"; "None" removes the second part
      (from today, as a new version); a blank cell changes nothing.
- [ ] Set shift: "No second part" clears both times; saved, the shift reads 06:00–10:00 again.

## Part AG. Deviation allowances; called to work on a holiday or leave day (24 Nov)

Use a copy of the data. EMP-1020 on a 10:00–18:00 shift (or read the times below against their own shift), on a policy
with Late-mark grace 15, Late marks allowed in a month 0, After that limit "Half day of leave or loss of pay".
- [ ] Policies › the policy › Attendance › **Deviation allowances**: Early check-in, Late check-in, Early check-out, each
      Off with empty Minutes allowed and Times allowed in a month. Switch Late check-in on with Minutes 120 and no Times,
      Save: refused ("Late check-in is on: enter the minutes allowed and the times allowed in a month, or switch it
      off"). Times 2, Save: saved; History shows "Late check-in: on", "minutes allowed", "times allowed in a month".
- [ ] Punch (or MIS upload) four past days of this month at 11:00–19:00. My time › My attendance (and Attendance ›
      Records › By employee › EMP-1020): the first two are **Present** with "Late check-in allowed, 1 h (1 of 2)" and
      "(2 of 2)"; the 3rd and 4th are **Late** with "Late check-in 1 h, 3rd this month (allowed 2): beyond the
      allowance". Pills "2 late check-in allowed", "2 beyond the deviation allowance". Loss of pay counts only the two
      late marks (1 day), not four (2 days).
- [ ] A day at 12:30 (150 min late) is a Late mark noted "more than the 2 h allowed" and does not use up the count.
      A day at 11:00–18:00 within the count is a Half day (7 h): no hours are credited.
- [ ] As HR Admin (and as EMP-1020's manager): the bell has "… (EMP-1020): late check-in 1 h on <date>, 3rd this month
      (allowed 2)" for a day today or in the last two days, once (it does not repeat every 5 minutes), with Attendance
      alerts all off.
- [ ] Regularize one of the allowed days: it no longer counts; the next late day takes its place in the count.
- [ ] Early check-in on with 120 and 1; set the shift's Attendance based on Shift (hours from the shift start). A day
      08:00–16:00: Present, 8 h, "Early check-in allowed, 2 h (1 of 1)", and no Early exit / Shift deviation in Roster
      discipline. A second such day: Half day (6 h from 10:00), reported.
- [ ] Early check-out on with 60 and 1: a day 10:00–17:00 is "Early check-out allowed, 1 h (1 of 1)" and no Early exit
      deviation; the next 10:00–17:30 is reported. Today's early check-out is only judged tomorrow.
- [ ] Switch all three off and save: every day reads as before (Late marks, Shift / Early exit deviations).
- [ ] Attendance › **Called to work** (HR Admin): Call to work EMP-1020 on a working day: refused ("… is not a holiday or
      a day of approved leave for …"). No reason: refused. A line manager sees the list without the Call to work and
      Remove buttons; My view has no such tab.
- [ ] Approve a Casual leave for EMP-1020 on a past day, note the balance on My leave, punch that day 10:00–18:05, then
      Call to work on it with a reason. The list shows "Worked"; My attendance shows the day **Present**, "Called to
      work, worked 8:05; Casual leave returned (1 day)"; My leave shows the balance 1 higher; the leave request (still
      Approved) has "Called to work on …" and, within 5 minutes, "Worked on a leave day … 1 day of Casual leave
      returned".
- [ ] A Loss of pay leave day called and worked: Present, the month's loss of pay is one day less. A half-day Casual
      leave called and worked: 0.5 returned.
- [ ] A holiday called and worked: Present (Called to work); loss of pay unchanged. Called and punched only in: Present,
      "Called to work, came (in …, no out punch)". Called and worked 3 h: Present. Called, no punch: Holiday,
      "Called to work, no punch: the holiday stands"; no loss of pay.
- [ ] A leave plan with Earn by working days: the called holiday counts 1 working day.
- [ ] Remove a call: the day goes back to the holiday / leave (balance back). Publish (or approve) that payroll month:
      adding or removing a call there is refused ("… is already paid …").

## Part AH. Individual roster changes: cell picker, called to work, other time, own deviation allowances (24 Nov)

Use a copy of the data. Shift and roster › Roster, a city and department with EMP-1020, a week with a holiday on their
calendar. HR Admin unless said otherwise.
- [ ] Click any cell: a picker opens (no more cycling) with each shift and its timing, WO, PH and Other time for this day.
      Pick another shift: the cell changes as before (late change reason, rest warning or refusal, lock of past dates).
- [ ] A holiday cell (PH from the calendar): the picker has WO and PH under Roster and a **Called to work** box with a
      Shift list and a Reason. Call to work with no reason: refused. With shift G and a reason: the cell shows **G***
      with a dashed border; the tooltip says "Called to work (holiday): G 09:00–18:00. <reason>. By …"; On duty counts
      them; Attendance › Called to work lists the call with "Shift G (from the roster)".
- [ ] Bulk upload a roster file that puts a shift on a calendar holiday: refused for that cell ("… is a holiday … A
      shift on a holiday calls them to work …"). Changing the called cell to WO: refused ("Remove the call to work
      first").
- [ ] A past holiday called with a shift, no punch: My attendance shows **Holiday**, "Called to work, no punch: the
      holiday stands", no loss of pay. Punch it (any punch): **Present**, measured against shift G.
- [ ] An approved Casual leave cell (L): the picker says to cancel the leave for roster changes and offers Called to work.
      Call with a shift, punch the day: Present; My leave shows the balance 1 higher. An approved Loss of pay day
      called and punched: its loss of pay goes.
- [ ] Open a called cell: **Remove the call** puts PH / L back (balance back). Choosing another shift and Save changes
      the call's shift. In a published payroll month the call and its removal are refused ("… is already paid …").
- [ ] As a Line Manager (roster edit, att.records View only): the Called to work box is not shown; the API answers 403.
- [ ] **Other time for this day** on a future working day: Start 10:00, End 14:00, tick Two parts, 17:00–21:00, Set
      other time: the cell shows **OT** (tooltip "Other time 10:00–14:00 + 17:00–21:00"), the footer has an Other time
      row, On duty counts it. Second part 13:00–21:00: refused ("The second part must start after the first part ends
      (14:00)"). A day in one part 22:00–06:00 is accepted (recorded like a night shift).
- [ ] On a past day (work policy Roster › past changes "free"): OT 06:00–10:00 + 15:00–19:00 with punches 06:00–17:00
      is a **Half day** (6 h: the 10:00–15:00 gap does not count); 06:30–19:30 is **Late**; 06:00–19:00 is **Present**.
      With the lock on, the past day is refused like any change. Inside the lead days, no reason: refused.
- [ ] With the work policy's Maximum early in punch set, an OT day's in punch outside the window is not taken (the
      punch window follows the OT start).
- [ ] Click EMP-1020's **name**: Deviation allowances shows the policy's rules and the ones in effect. Late check-in
      **On** with Minutes 120 and no Times: Save refused. Times 4, Applies from the 1st of the month: saved; History
      shows the version "In effect"; the row says "own deviation rules". With the policy at 120 minutes 2 times, four
      days 60 minutes late are all Present ("Late check-in allowed … (4 of 4)").
- [ ] Same date, Late check-in **Use the policy's rule**: back to 2 allowed, 2 late marks. **Off for this employee**: 4
      late marks, and the bell gets "beyond the allowance" messages for today and the last two days.
- [ ] Applies from a date in a published payroll month: refused. As a Line Manager: the dialog opens read-only (no
      Change from a date); the API refuses a save with 403.

## Part AI. Deviation allowances: the Action beyond the allowance (24 Nov)

Use a copy of the data. EMP-1020 on a 10:00–18:00 shift (read the times against their own shift), on a policy with
Late-mark grace 15, Late marks allowed in a month 0, After that limit "Half day of leave or loss of pay", Full-day 8 h,
Half-day 4 h.
- [ ] Policies › the policy › Attendance › Deviation allowances: the intro explains the four actions and "the harsher
      action wins"; each rule has an **Action** dropdown showing **Select**. Late check-in On, Minutes 120, Times 1, no
      Action, Save: refused ("Late check-in is on: choose the action for a day beyond the allowance, or switch it off").
      Pick **Half day until regularized**, Save: saved; History shows "Late check-in: action beyond the allowance".
- [ ] Punch three past days of this month 11:00–19:00. My time › My attendance: the 1st is Present ("Late check-in
      allowed, 1 h (1 of 1)"); the 2nd and 3rd are **Half day until regularized**, loss of pay 0.5 each, note "Late
      check-in 1 h, 2nd this month (allowed 1): beyond the allowance → Half day until regularized", with a
      **Regularize** button. Late marks: 0 (no late mark on top; nothing over the late-mark limit). Pill "2 half day
      until regularized".
- [ ] Action **Absent**: those days are **Absent until regularized**, loss of pay 1 each. **Present until regularized**:
      Present until regularized, no loss of pay, still with Regularize. **No action**: Late, two late marks over the
      limit (1 day), as before.
- [ ] Half day until regularized, a day 15:00–18:00 (5 h late, 3 h worked): **Absent (short hours)**, loss of pay 1 (not
      1.5), note "… → Half day until regularized; the hours or punches give more loss of pay: Absent (short hours)", Regularize shown.
- [ ] Early check-out On, 30 minutes, 5 times, Action Absent; Late check-in 30, 5, Present until regularized. A day
      11:00–17:00: **Absent until regularized** (the harsher action wins).
- [ ] Attendance › Issues (HR Admin) lists the days until regularized with Regularize. Raise a regularization for one
      of them older than the regularization window: refused ("Regularization is allowed within … days"). Within the
      window (or with an exception): send, approve; the day becomes **Present** (source Regularized), no loss of pay;
      the next day beyond keeps its action (the count does not move to it if it was beyond the minutes).
- [ ] The bell to HR Admins and the manager ends with "→ Half day until regularized" (or the chosen action).
- [ ] Payroll cut-off reminder (Attendance alerts on): Half day until regularized days are in its list.
- [ ] Roster › click EMP-1020's name: the policy's rule reads "on, 30 min, 5 times a month, beyond: …". Late check-in
      **On** for this employee with Minutes and Times but no Action: refused. With Action **Absent**: their late days
      beyond are Absent until regularized while the policy says Half day. **Use the policy's rule**: back to the
      policy's action.
- [ ] Payroll for the month (copy only): loss of pay matches the register; the half days count in Half day, the absent
      ones in No punch, the present ones in Present.

## Part AJ. Shifts: no Attendance rules card; Department + Location mapping with bulk upload (24 Nov)

Masters › Shifts, the roster, Onboarding. Use a copy of the data.
- [ ] Set shift has no **Attendance rules** card (no Max Hrs:mins, Attendance based on, Absent / Half day / Full day
      rule, Weekly or Monthly adjustment, Inclusion in shortfall hours, no "Not here yet" line). The header says "The
      work policy decides the attendance rules (policy name, N on this shift)". The shift list has no Rules column;
      Details ends with "Applies to: …".
- [ ] Weekly and monthly adjustments, "Attendance based on" and the max-hours cap are gone from the product: a day of
      6 h on an 8 h / 4 h policy is a Half day whatever the shift; no day reads "Short hours".
- [ ] After migration 029 every shift lists the same people as before: a shift with All locations reads "All
      departments at all locations"; one with selected locations has one row "All departments at <location>" each.
      Roster codes and the Onboarding shift picker offer the same shifts as before for every employee.
- [ ] Add shift: Save without a row is refused ("Add at least one row …"). Add "Front Office at all locations", Save:
      the shift is created with that row; Applies to: history shows "Added with the shift".
- [ ] Set shift › Applies to: add "All departments at Noida"; adding it again is refused ("… is already on this
      shift"). Switch a row off and on; Remove a row (confirm). Each change shows a toast and a history line with who
      and when. Switching off or removing the last switched-on row is refused ("This is the shift's last row
      switched on …").
- [ ] Map shift N to Front Office at one location only. On the roster, a Front Office employee there is offered N in
      the cell picker; a Housekeeping employee at the same location is not; their default shift and planned cells do
      not change if they were already on N (the roster warns "Shift N is not mapped to this department and
      location"). Setting N on their cell is refused: "Shift N does not apply to Housekeeping at …".
- [ ] The roster download has one drop-down per department and property block; uploading N for the Housekeeping
      employee is refused ("“N” is not a shift mapped to Housekeeping at …").
- [ ] Onboarding offer and joining verification: the Shift list shows only shifts mapped to the chosen department and
      location. Employee › job change to a shift, location or department the rows do not allow is refused; changing
      something else (grade) for someone outside the rows still saves. Import employees refuses an unmapped shift.
- [ ] Masters › Shifts › **Bulk upload** opens a dialog (no move to Bulk update): Download template, Download current
      data (Shift Code, Department, Location, Active for the rows on now), Check, Apply. Rows: "N,Front Office,<loc>,"
      adds or switches on; "G,All,All,No" switches off; "G,,," switches it back on; an unknown shift, department or
      location, "Maybe" in Active, a row not on the shift with No, and the same row twice are refused with the reason.
      Bulk update lists the same template under Leave and time › Shift mapping.
- [ ] Master update workbook › Shifts: the old Locations column is "Applies to (read-only …)" listing the rows; edits
      in it are ignored. A shift added in the workbook applies to all departments at all locations.

## Part AK. Retainer TDS 1%, reimbursement outside the gross, portal code = ID number, RET codes (25 Nov)

Use a copy of the data (migration `030_retainer_tds_1pct` runs on it).
1. **Retainer TDS.**
- [ ] Compensation › Salary structure › Version 1: "Retainer TDS" shows **TDS, % of gross (editable)** = 1 (was 10;
      Audit log has "Retainer TDS set to 1% (was the old default 10%)"). A structure set to anything other than 10 keeps it.
- [ ] Salary calculator, type Retainer, gross ₹30,000: Retainer fee 30,000, TDS 300 ("1% of gross (editable: structure
      or employee)"), net 29,700. No text says "section 194J" or 10%.
- [ ] Employees › a retainer › pay section shows "TDS, % of gross (editable)": "1% of gross (the structure's rate)".
      Edit pay options: type 3, choose a reason, save → "3% of gross (set for this employee)", Change history "TDS %:
      Structure rate → 3% of gross". Payroll dry run: the retainer's TDS is 3% of the earned fee; payslip "TDS, 3% of
      gross". Clear the box and save → back to the structure's rate. 60 is refused; a permanent employee has no box.
2. **Reimbursement outside the gross.** A permanent employee on Version 1, gross ₹30,000 › Edit pay options.
- [ ] Reimbursement, ₹ a month (outside the gross) 3,000: preview Basic 15,000, HRA 7,500, Special allowance 7,500,
      Gross 30,000, then "Reimbursement, outside the gross" 3,000; PF on Basic only (1,800), ESI/bonus as without it; net
      and CTC higher by 3,000. 12,000 is accepted too (no "what Basic and HRA leave" limit). Pay section says "₹3,000 a
      month, paid on top of the gross".
- [ ] Payroll dry run: payslip lists Basic, HRA, Special allowance, then "Reimbursement — Outside the gross"; a person
      with 10 of 30 days LOP gets 2,000 (by days paid, like the gross). Payroll sheet column "Reimbursement (outside the
      gross)" after Special allowance; full register the same; ESI/PF wages unchanged.
- [ ] Cost and budget: the salary figure includes it (hover: "Plus reimbursement outside the gross"); Excel column
      "Reimbursement (outside the gross)". My tax › Estimated tax shows "Reimbursement a month, outside the gross".
- [ ] Offer › Compensation "Reimbursement, ₹ a month (outside the gross)" 3,000: the offer letter breakup has
      "Reimbursement (outside the gross): ₹3,000 a month"; Mark joined carries it to the employee.
3. **Realtime portal code = the ID's number, 6 digits** (rtfake / a test portal only; never the live portal).
- [ ] Mark joined JI-0017 (mapped): the create sends Employee code **000017** (card 00000017); the employee's Attendance
      code becomes 000017 and pulled punches with EmpCode 17 or 000017 match. EMP-1054 → 001054.
- [ ] If another employee already has that Attendance code, the create is held with "Attendance code … is already …'s:
      set …'s Attendance code by hand (Employees) if they are already on the portal".
4. **Employee codes by type** (Employees › Employee codes: one series for everyone, EMP-, 4 digits, next 1055).
- [ ] Give a Permanent an Employee ID → EMP-1055; then a Retainer → RET-1056; then a Permanent → EMP-1057 (one counter,
      same width). Contractual → EMP-…. Import employees without a code: the same. "Next code" for type Retainer shows RET-….

## Part AL. Offer at joining (26 Nov)

1. **Choose.** Select a candidate (interview Selected), open it, Offer › **Offer at joining**: expected date today, note "UT hire".
- [ ] Pipeline has an **Offer at joining** filter with a count; the row says "Offer at joining: waiting for the joining day, expected on …".
- [ ] Timeline "Offer at joining chosen"; Mark joined is not offered. A past date is refused.
- [ ] With a date in the future, "Raise offer at joining" is disabled until that day; **Raise offer v1 instead** switches back (timeline "Switched back to the usual offer").
2. **Raise offer at joining** on the day: the usual offer form opens with the joining date prefilled; send it.
- [ ] The candidate says "Offer at joining v1: waiting for offer approval"; Joining says "Mark joined: Waiting for offer approval…".
- [ ] The offer screen has no Release button and explains the letter is given at joining.
3. **Approve** the offer in Approvals as usual, then **Mark joined** today.
- [ ] The candidate gets JI-…; the Offer ID (OI-…) appears; offer version v1 shows Accepted · frozen; the offer screen shows the letter "Released at joining on …".
- [ ] Timeline: Offer at joining chosen → Offer v1 raised at joining → Offer v1 approved → Offer accepted at joining → Offer letter released at joining → Joined.
4. **Outcomes.** Another offer-at-joining candidate: Reschedule joining moves the expected date; No show (on the day) starts the waiting window; Offer declined closes it.
- [ ] The normal path (Raise offer v1 → approval → release → Accepted → Mark joined) works exactly as before.

## Part AM. All-inclusive gross from a month (26 Nov)

1. **Setting.** Compensation › Salary structure › All-inclusive gross from month: pick the open payroll month, Save.
- [ ] Pill "On from …"; a month already approved or published is refused; Switch off is refused once a month on the rule is approved.
- [ ] Someone without Payroll settings (Edit) sees the month but cannot change it. Audit log has the change.
2. **Breakup.** Try an offer, Permanent, gross 18,000 (at or below the bonus ceiling), Haryana.
- [ ] Basic 8,708.45, HRA 4,354.23, Special allowance 4,354.23, Statutory bonus 583.10, Gross salary 18,000.
- [ ] Deductions: Employee PF 1,045, Employer PF 1,045, Employee ESI 131, Employer ESI 567, EDLI 43.54, LWF 34, Employer LWF 68; net 15,066.46.
- [ ] CTC: 18,000 + admin 43.54 + gratuity 418.88 + PL encashment 362.85 = 18,825.27 (+ variable target + reimbursement).
- [ ] Gross 28,000: no bonus line, no ESI; Basic 14,000.
3. **Payroll.** Dry run the month: payslip lists Statutory bonus after Special allowance, Employer PF/ESI/EDLI/LWF under deductions, variable pay "Pro-rata, additional" with no amount entered.
- [ ] Payroll › Variable pay for that month says variable pay is automatic; entering an amount (screen or bulk) is refused.
- [ ] Sheet/register: Statutory bonus, Gross salary (G), employer shares deducted; cost console shows no employer PF/ESI for the month.
- [ ] The earlier (published) month's payslip and register are unchanged.

## Part H. Known, deliberate differences from the prototype (not bugs)

- Left out because they are demo-only: "Viewing as", "Simulate no response", "Load a 10-employee test", and the
  prototype's sample-data wording.
- The Payroll Manager approves payroll; Payroll cannot approve its own run (the prototype allows it).
- Finance and Finance Head have downloads off by default (spec 6.3).
- Nominee details are masked for Payroll (spec); the prototype shows "Hidden".
- There is no ₹10,000 claims sentence (the spec uses the ₹5,000 and ₹25,000 bands).
- No "Planned next" card on Pay schedule.
- Kept extras:
  - the Half day option in the leave dialog;
  - the Masters "Company" tab;
  - the Personal details and Opening leave balances bulk templates;
  - the first payroll month field;
  - type-ahead employee pickers.
- The PIP approval flow is HRBP, then HRBP Corporate.

---

## Bug template

```
Bug #:      (number it, and put it next to the checkbox)
Where:      page › tab › dialog   (e.g. Payroll › Payroll run › Adjust)
Signed in:  EMP-xxxx as <role>
Steps:      1. … 2. … 3. …
Expected:   what should happen (quote the prototype if it is a look/wording issue)
Got:        what happened (exact message; screenshot; console error if any)
Data:       request or employee IDs involved (e.g. LV-3005, EMP-1020, November 2026)
```
