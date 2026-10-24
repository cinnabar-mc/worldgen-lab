use super::*;
use steel_core::bootstrap::init_globals;
use steel_core::chunk::chunk_holder::ChunkHolder;
use steel_core::worldgen::EmptyChunkGenerator;
use steel_registry::vanilla_blocks;
use steel_utils::BlockStateId;

/// Opens a RAM-only epoch with generation stopped so each test controls chunk readiness.
async fn test_epoch(pool: &Arc<rayon::ThreadPool>) -> Epoch {
    init_globals();
    let generator = Arc::new(ChunkGeneratorType::Empty(EmptyChunkGenerator::new()));
    let damage = Arc::new(DamageHistory::default());
    let epoch = Epoch::open(0, &generator, &damage, pool).await;
    epoch.world.chunk_map.stop_generation_refill_loop();
    epoch.world.chunk_map.task_tracker.close();
    epoch.world.chunk_map.task_tracker.wait().await;
    epoch
}

/// Adds one ticketed position with a submission time and a completion channel per caller.
fn request(
    epoch: &mut Epoch,
    pos: (i32, i32),
    since: Instant,
    callers: usize,
) -> Vec<mpsc::Receiver<ChunkBytes>> {
    let (waiters, replies) = (0..callers).map(|_| mpsc::sync_channel(1)).unzip();
    let handle = epoch.world.chunk_map.request_chunk(
        ChunkPos::new(pos.0, pos.1),
        ChunkStatus::Full,
        ChunkTicketKind::Player,
    );
    let previous = epoch.pending.insert(
        pos,
        Pending {
            handle,
            waiters,
            since,
        },
    );
    assert!(previous.is_none());
    replies
}

/// Marks an empty Full fixture with a distinct first block and records its bridge bytes.
fn ready_chunk(holder: &ChunkHolder, state: BlockStateId) -> Vec<u8> {
    let chunk = holder.try_chunk(ChunkStatus::Full).expect("Full fixture");
    chunk.sections().set_relative_block(0, 0, 0, state);
    encode(chunk)
}

/// Counts jobs needed by an action while the pool's sole worker is already occupied.
/// Cooperative yielding lets an unexpected install finish and fail the test without a timeout.
fn submitted_jobs(pool: &rayon::ThreadPool, action: impl FnOnce()) -> usize {
    assert_eq!(pool.current_num_threads(), 1);
    let (started, occupied) = mpsc::sync_channel(1);
    let (finish, finished) = mpsc::channel();
    let (report, count) = mpsc::sync_channel(1);
    pool.spawn(move || {
        started.send(()).expect("observer started");
        let mut submitted = 0;
        while matches!(finished.try_recv(), Err(mpsc::TryRecvError::Empty)) {
            if matches!(rayon::yield_now(), Some(rayon::Yield::Executed)) {
                submitted += 1;
            } else {
                std::thread::yield_now();
            }
        }
        let _ = report.send(submitted);
    });
    occupied.recv().expect("worker occupied");
    action();
    finish.send(()).expect("observer still occupied");
    count.recv().expect("observer job count")
}

#[tokio::test]
async fn empty_completion_skips_occupied_pool_and_expires_waiters() {
    let pool = Arc::new(
        rayon::ThreadPoolBuilder::new()
            .num_threads(1)
            .build()
            .unwrap(),
    );
    let mut epoch = test_epoch(&pool).await;
    let expired = request(&mut epoch, (0, 0), Instant::now() - REQUEST_DEADLINE, 2);
    let pending = request(&mut epoch, (1, 0), Instant::now(), 1);

    let submitted = submitted_jobs(&pool, || epoch.complete(&pool));

    assert_eq!(epoch.ticks, 1);
    for reply in expired {
        assert_eq!(
            reply.try_recv().unwrap(),
            Err("Steel chunk generation timed out".to_owned())
        );
        assert_eq!(reply.try_recv(), Err(mpsc::TryRecvError::Disconnected));
    }
    assert_eq!(pending[0].try_recv(), Err(mpsc::TryRecvError::Empty));
    assert_eq!(epoch.waiting(), 1);
    assert_eq!(epoch.pending.len(), 1);
    assert!(epoch.pending.contains_key(&(1, 0)));
    assert!(epoch.retained.is_empty());
    assert!(epoch.footprint.is_empty());
    assert!(epoch.recent.is_empty());
    close_world(&epoch.world).await;
    assert_eq!(submitted, 0);
}

#[tokio::test]
async fn nonempty_completion_preserves_bytes_tickets_and_all_waiters() {
    let pool = Arc::new(
        rayon::ThreadPoolBuilder::new()
            .num_threads(1)
            .build()
            .unwrap(),
    );
    let mut epoch = test_epoch(&pool).await;
    let holders = epoch
        .world
        .chunk_map
        .generate_region(ChunkPos::new(0, 0), ChunkPos::new(1, 0))
        .await
        .expect("generate two empty Full fixtures");
    let first_bytes = ready_chunk(&holders[0], vanilla_blocks::STONE.default_state());
    let second_bytes = ready_chunk(&holders[1], vanilla_blocks::DIRT.default_state());
    assert_ne!(first_bytes, second_bytes);
    let first = request(&mut epoch, (0, 0), Instant::now() - REQUEST_DEADLINE, 2);
    let second = request(&mut epoch, (1, 0), Instant::now(), 1);
    let pending = request(&mut epoch, (2, 0), Instant::now(), 1);
    let expired = request(&mut epoch, (3, 0), Instant::now() - REQUEST_DEADLINE, 1);

    // This positive control also proves the observer detects a nonempty pool install.
    let submitted = submitted_jobs(&pool, || epoch.complete(&pool));

    let first_reply = first[0].try_recv().unwrap().unwrap();
    let duplicate_reply = first[1].try_recv().unwrap().unwrap();
    assert_eq!(&*first_reply, first_bytes.as_slice());
    assert!(Arc::ptr_eq(&first_reply, &duplicate_reply));
    assert_eq!(
        &*second[0].try_recv().unwrap().unwrap(),
        second_bytes.as_slice()
    );
    for reply in first.iter().chain(&second) {
        assert_eq!(reply.try_recv(), Err(mpsc::TryRecvError::Disconnected));
    }
    assert_eq!(pending[0].try_recv(), Err(mpsc::TryRecvError::Empty));
    assert_eq!(
        expired[0].try_recv().unwrap(),
        Err("Steel chunk generation timed out".to_owned())
    );
    assert_eq!(expired[0].try_recv(), Err(mpsc::TryRecvError::Disconnected));
    assert_eq!(epoch.ticks, 1);
    assert_eq!(epoch.waiting(), 1);
    assert_eq!(epoch.pending.len(), 1);
    assert!(epoch.pending.contains_key(&(2, 0)));
    assert_eq!(epoch.retained.len(), 2);
    for pos in [(0, 0), (1, 0)] {
        let handle = &epoch.retained[&pos];
        assert_eq!(handle.positions(), &[ChunkPos::new(pos.0, pos.1)]);
        assert_eq!(handle.status(), Some(ChunkStatus::Full));
        assert_eq!(handle.poll(), ChunkRequestState::Ready);
        assert_eq!(handle.ticket_kind(), Some(ChunkTicketKind::Player));
        for dx in -FULL_RADIUS..=FULL_RADIUS {
            for dz in -FULL_RADIUS..=FULL_RADIUS {
                assert!(epoch.footprint.contains(&(pos.0 + dx, pos.1 + dz)));
            }
        }
    }
    assert_eq!(epoch.recent.len(), 2);
    assert_eq!(
        epoch.recent.iter().copied().collect::<FxHashSet<_>>(),
        FxHashSet::from_iter([(0, 0), (1, 0)])
    );
    assert!(epoch.warm.is_none());
    close_world(&epoch.world).await;
    assert_eq!(submitted, 1);
}
