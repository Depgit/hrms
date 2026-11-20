// Known volatility: values that differ between two runs of the SAME build (found by diffing two Postgres runs
// against each other) and are not covered by the normalizer. Keep this list short and give the reason for each
// rule; never add a rule to hide a difference between the Postgres and the MongoDB build.
//
// A rule: { key: RegExp on the call key ("phase | role-or-step | METHOD /path"), path: RegExp on the JSON path with
// array indexes written as [] (e.g. "$.rows[].id"), action, why }
//   action 'ignore'    — the value at the path is replaced by "<volatile>" on both sides
//   action 'unordered' — the array at the path is compared as a multiset (sorted canonically on both sides)
export const RULES = [
  {
    key: /\| GET \/api\/roster\/discipline\b/, path: /^\$\.managers$/, action: 'unordered',
    why: 'roster.go discipline(): managers come from a Go map and are ordered with sort.Slice (not stable) by the planning score, so managers with the same score come out in any order, in the Postgres build too',
  },
  {
    key: /\| GET \/api\/dashboard\?tab=pay\b/, path: /^\$\.latest\.byCity$/, action: 'unordered',
    why: 'dashboard.go (payroll tab): latest.byCity is built by ranging over a Go map, so its order is random in the Postgres build too',
  },
  {
    key: /\| GET \/api\/access\/person\//, path: /^\$\.named$/, action: 'unordered',
    why: 'access_more.go accessPerson(): the "<role> for <location/department>" and "(company-wide)" entries come from ranging over the Go maps People and Company, so someone named for several roles in one place gets them in random order, in the Postgres build too',
  },
  {
    key: /\| onb\.check-bad \| POST \/api\/candidates\/check$/, path: /^\$\.errors$/, action: 'unordered',
    why: 'onboarding_stages.go candidateCheck(): the "… is required" errors for city, location, department and designation come from ranging over a Go map, so their order is random in the Postgres build too',
  },
];
