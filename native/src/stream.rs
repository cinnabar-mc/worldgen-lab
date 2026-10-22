//! Per-chunk streaming from a long-lived world that reuses neighbouring generation work.
//!
//! Feature placement order between neighbours follows task completion, as in Java's chunk
//! map, so a chunk can differ slightly between worlds; one world never changes a served chunk.
use crate::encoding::encode;
use crate::host::{ChunkBytes, Job, close_world, open_world};
use rayon::prelude::*;
use rustc_hash::{FxHashMap, FxHashSet};
use std::collections::VecDeque;
use std::sync::{Arc, mpsc};
use std::time::{Duration, Instant};
use steel_core::chunk::chunk_pyramid::GENERATION_PYRAMID;
use steel_core::chunk::chunk_request::{
    ChunkRequest, ChunkRequestHandle, ChunkRequestState, ChunkTicketKind,
};
use steel_core::chunk::status::ChunkStatus;
use steel_core::entity::damage::DamageHistory;
use steel_core::world::World;
use steel_core::worldgen::ChunkGeneratorType;
use steel_utils::ChunkPos;
use tokio::sync::mpsc as queue;

/// Chunk-source phase interval while requests are outstanding; it bounds added latency.
const TICK: Duration = Duration::from_millis(2);
/// Positions within the Full dependency radius of a world's served chunks before a fresh
/// world replaces it; a radius-12 view flown straight reaches it after ~8,000 chunks, ~1.3 GB.
const WORLD_FOOTPRINT: usize = 16_384;
/// Waiting callers before the host stops reading new requests, keeping later callers blocked.
const MAX_PENDING: usize = 256;
/// Age at which a request fails rather than holding its caller and shutdown indefinitely;
/// shorter than the cgo caller's own timeout.
const REQUEST_DEADLINE: Duration = Duration::from_secs(100);
/// Longest a replacement world prepares before taking over anyway.
const WARM_DEADLINE: Duration = Duration::from_secs(10);
/// Radius of the halo a Full chunk keeps loaded.
const FULL_RADIUS: i32 = GENERATION_PYRAMID
    .get_step_to(ChunkStatus::Full)
    .get_accumulated_radius_of(ChunkStatus::Empty) as i32;
/// Recently served chunks a replacement world prepares, so the view's frontier keeps its halo.
/// Preparation stops at a quarter of WORLD_FOOTPRINT, so scattered chunks cannot refill it.
const WARM_CHUNKS: usize = 256;

/// An unfinished request and every caller waiting for it.
struct Pending {
    handle: ChunkRequestHandle,
    waiters: Vec<mpsc::SyncSender<ChunkBytes>>,
    since: Instant,
}

/// One world and the tickets that keep its requested chunks and their halos loaded.
struct Epoch {
    world: Arc<World>,
    pending: FxHashMap<(i32, i32), Pending>,
    retained: FxHashMap<(i32, i32), ChunkRequestHandle>,
    warm: Option<ChunkRequestHandle>,
    footprint: FxHashSet<(i32, i32)>,
    recent: VecDeque<(i32, i32)>,
    ticks: u64,
}

impl Epoch {
    /// Opens a world with no tickets.
    async fn open(
        seed: i64,
        generator: &Arc<ChunkGeneratorType>,
        damage: &Arc<DamageHistory>,
        pool: &Arc<rayon::ThreadPool>,
    ) -> Self {
        Self {
            world: open_world(seed, Arc::clone(generator), damage, pool).await,
            pending: FxHashMap::default(),
            retained: FxHashMap::default(),
            warm: None,
            footprint: FxHashSet::default(),
            recent: VecDeque::new(),
            ticks: 0,
        }
    }

    /// Counts the halo positions that `pos` keeps loaded.
    fn hold(&mut self, (x, z): (i32, i32)) {
        for dx in -FULL_RADIUS..=FULL_RADIUS {
            for dz in -FULL_RADIUS..=FULL_RADIUS {
                self.footprint.insert((x + dx, z + dz));
            }
        }
    }

    /// Advances ticket propagation and generation scheduling by one chunk-source phase.
    fn tick(&mut self) {
        self.world
            .chunk_map
            .tick_game(&self.world, self.ticks, 0, false);
        self.ticks += 1;
    }

    /// Waiting callers across all unfinished requests.
    fn waiting(&self) -> usize {
        self.pending
            .values()
            .map(|pending| pending.waiters.len())
            .sum()
    }

    /// Ticks, replies to every request whose chunk reached Full and keeps one ticket per
    /// position, then fails requests older than REQUEST_DEADLINE.
    fn complete(&mut self, pool: &rayon::ThreadPool) {
        self.tick();
        let ready: Vec<_> = self
            .pending
            .iter()
            .filter_map(|(pos, pending)| {
                let ready = pending.handle.ready_chunks()?;
                Some((*pos, Arc::clone(&ready.holders[0])))
            })
            .collect();
        let encoded: Vec<_> = pool.install(|| {
            ready
                .par_iter()
                .map(|(pos, holder)| {
                    let bytes = holder
                        .try_chunk(ChunkStatus::Full)
                        .map(|chunk| Arc::<[u8]>::from(encode(chunk)))
                        .ok_or_else(|| "Full chunk disappeared before encoding".to_owned());
                    (*pos, bytes)
                })
                .collect()
        });
        for (pos, bytes) in encoded {
            let Pending {
                handle, waiters, ..
            } = self.pending.remove(&pos).expect("ready request");
            self.retained.entry(pos).or_insert(handle);
            self.hold(pos);
            if self.recent.len() == WARM_CHUNKS {
                self.recent.pop_front();
            }
            self.recent.push_back(pos);
            for waiter in waiters {
                let _ = waiter.send(bytes.clone());
            }
        }
        for (_, pending) in self
            .pending
            .extract_if(|_, pending| pending.since.elapsed() >= REQUEST_DEADLINE)
        {
            for waiter in pending.waiters {
                let _ = waiter.send(Err("Steel chunk generation timed out".to_owned()));
            }
        }
    }
}

/// Serves each request as soon as its chunk is Full, while the world keeps its neighbours.
pub(crate) async fn serve(
    seed: i64,
    generator: Arc<ChunkGeneratorType>,
    pool: Arc<rayon::ThreadPool>,
    damage: Arc<DamageHistory>,
    mut jobs: queue::Receiver<Option<Job>>,
) {
    let mut live = Epoch::open(seed, &generator, &damage, &pool).await;
    let mut retiring: Vec<Epoch> = Vec::new();
    let mut warming: Option<(Epoch, Instant)> = None;
    let mut accepting = true;
    while accepting || !live.pending.is_empty() || !retiring.is_empty() {
        let busy = !live.pending.is_empty() || !retiring.is_empty() || warming.is_some();
        let outstanding = live.waiting() + retiring.iter().map(Epoch::waiting).sum::<usize>();
        let reading = accepting && outstanding < MAX_PENDING;
        let mut incoming = Vec::new();
        if !reading {
            tokio::time::sleep(TICK).await;
        } else if busy {
            tokio::select! {
                job = jobs.recv() => incoming.push(job.flatten()),
                () = tokio::time::sleep(TICK) => {}
            }
        } else {
            incoming.push(jobs.recv().await.flatten());
        }
        while reading
            && outstanding + incoming.len() < MAX_PENDING
            && let Ok(job) = jobs.try_recv()
        {
            incoming.push(job);
        }
        for job in incoming {
            let Some(job) = job else {
                accepting = false;
                continue;
            };
            if let Some(pending) = retiring
                .iter_mut()
                .chain(std::iter::once(&mut live))
                .find_map(|epoch| epoch.pending.get_mut(&job.pos))
            {
                pending.waiters.push(job.reply);
                continue;
            }
            let handle = live.world.chunk_map.request_chunk(
                ChunkPos::new(job.pos.0, job.pos.1),
                ChunkStatus::Full,
                ChunkTicketKind::Player,
            );
            live.pending.insert(
                job.pos,
                Pending {
                    handle,
                    waiters: vec![job.reply],
                    since: Instant::now(),
                },
            );
        }
        for epoch in retiring.iter_mut().chain(std::iter::once(&mut live)) {
            if !epoch.pending.is_empty() {
                epoch.complete(&pool);
            }
        }
        for epoch in retiring.extract_if(.., |epoch| epoch.pending.is_empty()) {
            close_world(&epoch.world).await;
        }
        if warming.is_none() && accepting && live.footprint.len() >= WORLD_FOOTPRINT {
            let mut next = Epoch::open(seed, &generator, &damage, &pool).await;
            for &pos in live.recent.iter().rev() {
                if next.footprint.len() >= WORLD_FOOTPRINT / 4 {
                    break;
                }
                next.hold(pos);
                next.recent.push_front(pos);
            }
            next.warm = Some(
                next.world.chunk_map.request_chunks(ChunkRequest {
                    status: ChunkStatus::Features,
                    positions: next
                        .recent
                        .iter()
                        .map(|&(x, z)| ChunkPos::new(x, z))
                        .collect(),
                    ticket_kind: ChunkTicketKind::Pregen,
                }),
            );
            warming = Some((next, Instant::now()));
        }
        if let Some((next, since)) = &mut warming {
            next.tick();
            let warmed = next
                .warm
                .as_ref()
                .is_none_or(|handle| handle.poll() == ChunkRequestState::Ready);
            if warmed || since.elapsed() >= WARM_DEADLINE {
                let (next, _) = warming.take().expect("warming world");
                retiring.push(std::mem::replace(&mut live, next));
            }
        }
    }
    for epoch in retiring
        .into_iter()
        .chain(std::iter::once(live))
        .chain(warming.map(|(epoch, _)| epoch))
    {
        close_world(&epoch.world).await;
    }
}
