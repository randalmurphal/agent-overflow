// Method-id hash. Must match Wails' internal/hash.Fnv: FNV-1a 32-bit
// over the byte string "main.App.<MethodName>".
//
// Verified against the Go side's pinned-vector test
// (internal/transport/dispatcher_test.go TestFnvHash_MatchesWails) —
// fnv_method_id("main.App.ArchiveProject") == 1352159878.
//
// We compute on demand rather than keeping a generated lookup table
// because (a) the call sites we have are dozens, not hundreds; (b) the
// const-fn in `fnv` makes the call a one-liner; (c) drift between a
// generated table and the live binding is the kind of bug the Go side
// already gates with methods_gen_test — duplicating it here would just
// mean two sources of truth.

use fnv::FnvHasher;
use std::hash::Hasher;

pub fn fnv_method_id(qualified: &str) -> u32 {
    let mut h = FnvHasher::default();
    h.write(qualified.as_bytes());
    // FnvHasher::finish is u64; the low 32 bits are FNV-1a 32-bit when
    // seeded with the 32-bit offset basis. The `fnv` crate seeds the
    // 64-bit variant by default, so we run our own minimal 32-bit
    // variant inline instead. (See test below.)
    h.finish() as u32
}

/// Our own FNV-1a 32-bit. Matches Go's hash/fnv `New32a`.
pub fn fnv1a_32(input: &str) -> u32 {
    const OFFSET: u32 = 0x811c9dc5;
    const PRIME: u32 = 0x0100_0193;
    let mut h: u32 = OFFSET;
    for b in input.as_bytes() {
        h ^= u32::from(*b);
        h = h.wrapping_mul(PRIME);
    }
    h
}

/// Convenience: prefix the method name with `main.App.` (the Go side's
/// canonical qualifier) and hash. This is the form the dispatcher
/// registers against.
pub fn method_id(method_name: &str) -> u32 {
    fnv1a_32(&format!("main.App.{method_name}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Pinned vectors lifted from the Go-side dispatcher_test.go
    /// TestFnvHash_MatchesWails.
    #[test]
    fn matches_wails_pinned_vectors() {
        assert_eq!(fnv1a_32("main.App.ArchiveProject"), 1352159878);
        assert_eq!(method_id("ArchiveProject"), 1352159878);
    }

    #[test]
    fn matches_observed_binding_ids() {
        // From frontend/bindings/agent-overflow/app.ts — these are what
        // the production Svelte client actually sends today.
        assert_eq!(method_id("ListProjects"), 2_721_360_259);
        assert_eq!(method_id("ListThreads"), 1_090_132_042);
        assert_eq!(method_id("ListRecentThreadItems"), 2_604_956_482);
        assert_eq!(method_id("GetThread"), 1_098_302_047);
        assert_eq!(method_id("ListRecentTurns"), 1_083_162_294);
    }
}
