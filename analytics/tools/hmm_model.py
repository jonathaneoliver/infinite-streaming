#!/usr/bin/env python3
"""#445 HMM core — shared model substrate for the latent-regime scorer.

The VOMM (`scorer.py`) answers "is this *ordering* of requests one we see in clean
traffic" (per-transition surprise). The HMM answers a different question: "what *regime*
is the player in, and is this regime transition normal" — a latent-state timeline
(STARTUP / STEADY / RECOVERING / STALLED / ENDING) decoded from the SAME cross-stream
token alphabet (`tokenize.py`). This module is the model itself; `hmm_deriver.py` wires it
into the #608 train/score split (→ derived_labels) and `hmm_scorer.py` wraps it for the
verification report (K-sweep / BIC / emission inspection).

CategoricalHMM (hmmlearn) over the token alphabet, fit with random restarts (Baum-Welch is
local-optimum prone). The fitted matrices + vocab + auto-assigned state labels + the
transition-surprise threshold serialize to a gzipped-JSON artifact — the model-agnostic
seam #608 left generic for exactly this. Scoring reconstructs the model from the artifact
(no retrain) and Viterbi-decodes each play into a regime timeline.

Dependencies (the deriver sidecar pip-installs these; the stdlib VOMM does not need them):
    hmmlearn numpy scikit-learn
"""
import collections
import gzip
import json
import math
import os
import tempfile
import warnings
from datetime import datetime, timezone

import numpy as np

# hmmlearn warns on non-convergence per restart; we keep the best restart ourselves.
warnings.filterwarnings("ignore", category=RuntimeWarning)
warnings.filterwarnings("ignore", message=".*did not converge.*")

DEFAULT_K = 5
DEFAULT_RESTARTS = 6
DEFAULT_MAX_ITER = 100
DEFAULT_TOL = 1e-4
# Post-fit smoothing floor so a token/transition unseen in training doesn't collapse a
# scored sequence's log-likelihood to -inf (and so Viterbi never hits a zero-prob wall).
PROB_FLOOR = 1e-6
MIN_TRAIN_SEQUENCES = 5      # a bucket with fewer clean plays than this can't train
SENTINELS = ("<S>", "<E>")


# ---------- token vocabulary ----------
class TokenVocab:
    """Stable token → contiguous index map. A reserved <UNK> at index 0 lets us score
    sequences containing tokens absent from training without retraining or reshaping the
    emission matrix."""

    UNK = "<UNK>"

    def __init__(self, idx2tok=None):
        if idx2tok:
            self.idx2tok = list(idx2tok)
        else:
            self.idx2tok = [self.UNK]
        self.tok2idx = {t: i for i, t in enumerate(self.idx2tok)}

    def fit(self, sequences):
        for seq in sequences:
            for tok in seq:
                if tok not in self.tok2idx:
                    self.tok2idx[tok] = len(self.idx2tok)
                    self.idx2tok.append(tok)
        return self

    def transform(self, seq):
        unk = self.tok2idx[self.UNK]
        return np.array([self.tok2idx.get(t, unk) for t in seq], dtype=np.int64)

    def n_tokens(self):
        return len(self.idx2tok)


# ---------- training ----------
def _stack(sequences, vocab):
    """List of token sequences → (X, lengths) as hmmlearn wants: X is (total_tokens, 1)
    ints, lengths is per-sequence token counts. Sequences shorter than 2 are dropped (no
    transition to learn)."""
    usable = [seq for seq in sequences if len(seq) >= 2]
    if not usable:
        return np.zeros((0, 1), dtype=np.int64), np.zeros((0,), dtype=np.int64)
    flat = np.concatenate([vocab.transform(seq) for seq in usable])
    lengths = np.array([len(seq) for seq in usable], dtype=np.int64)
    return flat.reshape(-1, 1), lengths


def _floor_and_renormalise(startprob, transmat, emissionprob, floor=PROB_FLOOR):
    """Clamp each probability array's zeros to `floor` and renormalise rows. Returns the
    three cleaned arrays. See PROB_FLOOR — protects out-of-sample scoring from -inf."""
    out = []
    for arr in (startprob, transmat, emissionprob):
        a = np.where(np.asarray(arr, dtype=float) < floor, floor, np.asarray(arr, dtype=float))
        a = a / a.sum() if a.ndim == 1 else a / a.sum(axis=1, keepdims=True)
        out.append(a)
    return out


def train_hmm(sequences, vocab, K=DEFAULT_K, restarts=DEFAULT_RESTARTS,
              max_iter=DEFAULT_MAX_ITER, tol=DEFAULT_TOL, seed=0):
    """Fit a CategoricalHMM with K hidden states; random-restart `restarts` times and keep
    the highest training log-likelihood (Baum-Welch is initial-condition sensitive).

    Returns (model, train_logL) or (None, -inf) when the bucket is too thin to fit."""
    from hmmlearn.hmm import CategoricalHMM

    X, lengths = _stack(sequences, vocab)
    if X.shape[0] < K * 5:                       # too few observations for K states
        return None, float("-inf")

    best, best_score = None, float("-inf")
    for r in range(restarts):
        rng = np.random.default_rng(seed + r)
        m = CategoricalHMM(
            n_components=K, n_features=vocab.n_tokens(), n_iter=max_iter, tol=tol,
            init_params="ste", random_state=int(rng.integers(0, 2**31 - 1)),
        )
        try:
            m.fit(X, lengths)
            sc = m.score(X, lengths)
        except (ValueError, RuntimeError):
            continue
        if sc > best_score:
            best, best_score = m, sc
    if best is not None:
        best.startprob_, best.transmat_, best.emissionprob_ = _floor_and_renormalise(
            best.startprob_, best.transmat_, best.emissionprob_)
    return best, best_score


def model_from_artifact(cond):
    """Rebuild a CategoricalHMM from a serialized condition dict (no retrain) for scoring."""
    from hmmlearn.hmm import CategoricalHMM

    startprob = np.asarray(cond["startprob"], dtype=float)
    transmat = np.asarray(cond["transmat"], dtype=float)
    emissionprob = np.asarray(cond["emissionprob"], dtype=float)
    m = CategoricalHMM(n_components=len(startprob), n_features=emissionprob.shape[1])
    m.startprob_, m.transmat_, m.emissionprob_ = startprob, transmat, emissionprob
    return m


def decode(model, vocab, seq):
    """Viterbi-decode one token sequence → (forward_logL, [state_idx]) or (None, None)."""
    if model is None or len(seq) < 2:
        return None, None
    X = vocab.transform(seq).reshape(-1, 1)
    try:
        ll, states = model.decode(X, algorithm="viterbi")
    except (ValueError, RuntimeError):
        return None, None
    return float(ll), states.tolist()


def bic(model, log_likelihood, n_observations):
    """BIC = -2·logL + k·log(N); k = (K-1) start + K(K-1) transition + K(V-1) emission free
    params. Lower is better — the K-sweep picks the K with the lowest aggregate BIC."""
    if model is None:
        return float("inf")
    K, V = model.n_components, model.n_features
    k = (K - 1) + K * (K - 1) + K * (V - 1)
    return -2.0 * log_likelihood + k * math.log(max(n_observations, 1))


# ---------- transition surprise (for unexpected_regime_transition) ----------
def transition_surprises(transmat, states):
    """[-log P(state_t → state_{t+1})] along a Viterbi path, under the learned transmat.
    out[j] is the surprise of the move LANDING on states[j+1]."""
    lt = np.log(np.asarray(transmat, dtype=float))
    return [float(-lt[states[i - 1], states[i]]) for i in range(1, len(states))]


def p99(values):
    return sorted(values)[min(len(values) - 1, int(0.99 * len(values)))] if values else 0.0


# ---------- post-hoc state interpretation (current tokenize.py alphabet) ----------
# Maps a hidden state to a human regime by inspecting its top emissions. The token families
# are the CURRENT ones (tokenize.py): V_SEG/A_SEG/V_PROBE, STARTUP_RAMP/LOOP_BOUNDARY,
# V_PL/A_PL/M_PL, FAULT(surface,class), and event markers STALL_*/BUF_*/RATE_*/FIRST_FRAME/
# SEGMENT_STALL/TIMEJUMP. Checked most-disruptive-first so a state only falls through to
# STEADY when nothing more specific dominates.
_SERVER_FAULT_CLASSES = ("5xx", "404", "auth", "4xx", "injected_reset", "corruption", "server_partial")


def label_state(top_tokens):
    """Return a regime label for a state given its top-emission token strings, or None to
    fall back to STATE_<i>."""
    def has(*subs):
        return any(any(s in t for s in subs) for t in top_tokens)

    server_fault = any(
        t.startswith("FAULT(") and any(c in t for c in _SERVER_FAULT_CLASSES) for t in top_tokens)
    if has("STALL_START", "SEGMENT_STALL") or server_fault:
        return "STALLED"
    if has("RATE_DOWN", "V_PROBE", "BUF_START", "LOOP_BOUNDARY", "STALL_END",
           "BUF_END", "TIMEJUMP") or any(t.startswith("FAULT(") for t in top_tokens):
        return "RECOVERING"
    if has("STARTUP_RAMP", "FIRST_FRAME", "M_PL", "V_PL", "<S>"):
        return "STARTUP"
    if has("<E>"):
        return "ENDING"
    if has("V_SEG", "A_SEG", "RATE_UP"):
        return "STEADY"
    return None


def top_emissions(emissionprob, vocab, state_idx, top_n=8):
    """[(token, prob)] — the state's highest-probability emissions, descending."""
    emit = np.asarray(emissionprob[state_idx], dtype=float)
    return [(vocab.idx2tok[i], float(emit[i])) for i in np.argsort(-emit)[:top_n]]


def auto_label_states(emissionprob, vocab, top_n=5):
    """Label every hidden state from its top emissions; disambiguate collisions with a
    numeric suffix so the timeline stays distinct (STALLED, STALLED_2, ...)."""
    raw = [label_state([t for t, _ in top_emissions(emissionprob, vocab, s, top_n)])
           or f"STATE_{s}" for s in range(len(emissionprob))]
    counts = collections.Counter(raw)
    seen, out = collections.Counter(), []
    for lbl in raw:
        if counts[lbl] == 1:
            out.append(lbl)
        else:
            seen[lbl] += 1
            out.append(f"{lbl}_{seen[lbl]}")
    return out


# ---------- run-length encoding of the state timeline ----------
def rle_intervals(states, state_labels, positions=None):
    """Collapse consecutive identical states into intervals. positions (optional, same
    length as states) lets the caller carry per-token anchors (e.g. (ts, fp, surface)) so
    the FIRST token of each interval can be located. Returns
    [(label, start_pos, end_pos, count, start_index)]."""
    if not states:
        return []
    pos = positions if positions is not None else list(range(len(states)))
    out, cur, start, start_i, count = [], states[0], pos[0], 0, 1
    for i in range(1, len(states)):
        if states[i] == cur:
            count += 1
            continue
        out.append((state_labels[cur], start, pos[i - 1], count, start_i))
        cur, start, start_i, count = states[i], pos[i], i, 1
    out.append((state_labels[cur], start, pos[-1], count, start_i))
    return out


def state_distribution(states, state_labels):
    c = collections.Counter(state_labels[s] for s in states)
    total = sum(c.values()) or 1
    return {lbl: n / total for lbl, n in c.most_common()}


# ---------- artifact I/O (gzipped JSON; atomic swap — mirrors derive_labels.py) ----------
def serialize_condition(model, vocab, train_logL, n_train_seqs, n_obs, trans_thr, labels):
    """One condition's fitted model + metadata → JSON-safe dict for the artifact."""
    return {
        "startprob": np.asarray(model.startprob_).tolist(),
        "transmat": np.asarray(model.transmat_).tolist(),
        "emissionprob": np.asarray(model.emissionprob_).tolist(),
        "idx2tok": vocab.idx2tok,
        "state_labels": labels,
        "K": int(model.n_components),
        "trans_thr": float(trans_thr),
        "train_logL": float(train_logL),
        "n_train_seqs": int(n_train_seqs),
        "n_observations": int(n_obs),
    }


def save_artifact(path, artifact):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", suffix=".tmp")
    os.close(fd)
    with gzip.open(tmp, "wt", encoding="utf-8") as f:
        json.dump(artifact, f, separators=(",", ":"))
    os.replace(tmp, path)            # atomic so the scorer never reads a half-written file
    return os.path.getsize(path)


def load_artifact(path):
    if not os.path.exists(path):
        return None
    with gzip.open(path, "rt", encoding="utf-8") as f:
        return json.load(f)


def utc_now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
