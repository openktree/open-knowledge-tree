// Concepts page page-size budget. The page is a flat list with no
// internal subcomponents, so the 150-line limit applies. The state
// is a single createResource + offset signal — well under the
// reactive-primitive threshold.
const PAGE_SIZE = 100;

export { PAGE_SIZE };
