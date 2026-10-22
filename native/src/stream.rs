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
use std::time::Duration;
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
/// world replaces it. Contiguous travel reaches it after about 8,000 chunks, near 1.3 GB.
const WORLD_FOOTPRINT: usize = 16_384;
/// Outstanding requests before the host stops reading new ones, keeping callers blocked.
const MAX_PENDING: usize = 256;
/// Radius of the halo a Full chunk keeps loaded.
const FULL_RADIUS: i32 = GENERATION_PYRAMID
    .get_step_to(ChunkStatus::Full)
    .get_accumulated_radius_of(ChunkStatus::Empty) as i32;
/// Recently served chunks a replacement world prepares, so the view's frontier keeps its halo.
const WARM_CHUNKS: usize = 256;

/// One world and the tickets that keep its requested chunks and their halos loaded.
struct Epoch {
    world: Arc<World>,
    pending: FxHashMap<(i32, i32), (ChunkRequestHandle, Vec<mpsc::SyncSender<ChunkBytes>>)>,
    retained: Vec<ChunkRequestHandle>,
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
            retained: Vec::new(),
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

    /// Ticks, then replies to every request whose chunk reached Full and keeps its ticket.
    fn complete(&mut self, pool: &rayon::ThreadPool) {
        self.tick();
        let ready: Vec<_> = self
            .pending
            .iter()
            .filter_map(|(pos, (handle, _))| {
                let ready = handle.ready_chunks()?;
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
            let (handle, waiters) = self.pending.remove(&pos).expect("ready request");
            self.retained.push(handle);
            self.hold(pos);
            if self.recent.len() == WARM_CHUNKS {
                self.recent.pop_front();
            }
            self.recent.push_back(pos);
            for waiter in waiters {
                let _ = waiter.send(bytes.clone());
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
    let mut warming: Option<(Epoch, ChunkRequestHandle)> = None;
    let mut accepting = true;
    while accepting || !live.pending.is_empty() || !retiring.is_empty() {
        let busy = !live.pending.is_empty() || !retiring.is_empty() || warming.is_some();
        let outstanding =
            live.pending.len() + retiring.iter().map(|e| e.pending.len()).sum::<usize>();
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
            if let Some((_, waiters)) = retiring
                .iter_mut()
                .chain(std::iter::once(&mut live))
                .find_map(|epoch| epoch.pending.get_mut(&job.pos))
            {
                waiters.push(job.reply);
                continue;
            }
            let handle = live.world.chunk_map.request_chunk(
                ChunkPos::new(job.pos.0, job.pos.1),
                ChunkStatus::Full,
                ChunkTicketKind::Player,
            );
            live.pending.insert(job.pos, (handle, vec![job.reply]));
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
            for &pos in &live.recent {
                next.hold(pos);
            }
            let handle = next.world.chunk_map.request_chunks(ChunkRequest {
                status: ChunkStatus::Features,
                positions: live
                    .recent
                    .iter()
                    .map(|&(x, z)| ChunkPos::new(x, z))
                    .collect(),
                ticket_kind: ChunkTicketKind::Pregen,
            });
            warming = Some((next, handle));
        }
        if let Some((next, handle)) = &mut warming {
            next.tick();
            if handle.poll() == ChunkRequestState::Ready {
                let (next, handle) = warming.take().expect("warming world");
                let old = std::mem::replace(&mut live, next);
                live.retained.push(handle);
                retiring.push(old);
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
