package gorker

import (
	"context"
	"math"
	"sync"

	"github.com/kpango/glg"
)

type Dispatcher struct {
	running     bool
	scaling     bool
	resizing    bool
	queue       []func()
	qin         chan func()
	qout        chan func()
	wg          *sync.WaitGroup
	mu          *sync.RWMutex
	workerCount int
	workers     []*worker
	ctx         context.Context
	cancel      context.CancelFunc
}

type worker struct {
	dis     *Dispatcher
	kill    chan struct{}
	running bool
}

var (
	defaultWorker   = 3
	bufferSizeLimit = 1000000.0
	instance        *Dispatcher
	once            sync.Once
)

func init() {
	instance = New(defaultWorker)
}

func GetInstance() *Dispatcher {
	return Get(defaultWorker)
}

func Get(maxWorker int) *Dispatcher {
	if maxWorker < 1 {
		maxWorker = 1
	}
	once.Do(func() {
		instance = New(maxWorker)
	})
	instance.workerCount = maxWorker
	if len(instance.workers) != maxWorker {
		instance.AutoScale()
	}
	return instance
}

func New(maxWorker int) *Dispatcher {
	if maxWorker < 1 {
		maxWorker = 1
	}
	dis := newDispatcher(maxWorker)
	for i := range dis.workers {
		dis.workers[i] = &worker{
			dis:     dis,
			kill:    make(chan struct{}, 1),
			running: false,
		}
	}
	return dis
}

func newDispatcher(maxWorker int) *Dispatcher {
	qs := 100000
	return &Dispatcher{
		running:     false,
		workerCount: maxWorker,
		queue:       make([]func(), 0, qs),
		qin:         make(chan func(), int(math.Min(float64(maxWorker*100), bufferSizeLimit))),
		qout:        make(chan func(), int(math.Min(float64(maxWorker*100), bufferSizeLimit))),
		wg:          new(sync.WaitGroup),
		mu:          new(sync.RWMutex),
		workers:     make([]*worker, maxWorker),
		ctx:         context.Background(),
	}
}

func (d *Dispatcher) QueueRunner() *Dispatcher {
	go func() {
		var job func()
		for {
			select {
			case <-d.ctx.Done():
				return
			case job = <-d.qin:
				d.mu.Lock()
				d.queue = append(d.queue, job)
				d.mu.Unlock()
			}
			if len(d.queue) > 0 {
				select {
				case d.qout <- d.queue[0]:
					d.mu.Lock()
					d.queue = d.queue[1:]
					d.mu.Unlock()
				}
			}
		}
	}()
	return d
}

func GetWorkerCount() int {
	return instance.GetWorkerCount()
}

// GetWorkerCount returns current worker count this function will be blocking while worker scaling
func (d *Dispatcher) GetWorkerCount() int {
	for {
		if !d.scaling && len(d.workers) == d.workerCount {
			return len(d.workers)
		}
	}
}

func (d *Dispatcher) ScaleBuffer(size int) *Dispatcher {
	size = int(math.Min(float64(size*100), bufferSizeLimit))
	d.mu.Lock()
	oldin := d.qin
	oldout := d.qout
	d.qin = make(chan func(), size)
	d.qout = make(chan func(), size)
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		tmpQueue := make([]func(), 0, len(oldin))
		for job := range oldin {
			tmpQueue = append(tmpQueue, job)
		}
		d.mu.Lock()
		d.queue = append(d.queue, tmpQueue...)
		d.mu.Unlock()
	}()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		tmpQueue := make([]func(), 0, len(oldout))
		for job := range oldout {
			tmpQueue = append(tmpQueue, job)
		}
		d.mu.Lock()
		d.queue = append(tmpQueue, d.queue...)
		d.mu.Unlock()
	}()
	return d
}

func UpScale(workerCount int) *Dispatcher {
	return instance.UpScale(workerCount)
}

func (d *Dispatcher) UpScale(workerCount int) *Dispatcher {
	d.ScaleBuffer(workerCount * 100)
	d.mu.Lock()
	d.scaling = true
	diff := workerCount - len(d.workers)
	for {
		if diff < 1 {
			break
		}
		d.workers = append(d.workers, &worker{
			dis:     d,
			kill:    make(chan struct{}, 1),
			running: false,
		})
		diff--
	}
	d.workerCount = workerCount
	d.mu.Unlock()
	if d.running {
		d.Start()
	}
	d.scaling = false
	return d
}

func DownScale(workerCount int) *Dispatcher {
	return instance.DownScale(workerCount)
}

func (d *Dispatcher) DownScale(workerCount int) *Dispatcher {
	d.ScaleBuffer(workerCount * 100)
	d.mu.Lock()
	d.scaling = true
	diff := len(d.workers) - workerCount
	idx := 0
	for {
		if diff < 1 {
			break
		}
		if d.running && d.workers[idx].running {
			d.workers[idx].stop()
		}
		d.workers = append(d.workers[:idx], d.workers[idx+1:]...)
		diff--
		idx++
		if idx >= len(d.workers) {
			idx = 0
		}
	}
	d.workerCount = workerCount
	d.scaling = false
	d.mu.Unlock()
	return d
}

func AutoScale() *Dispatcher {
	return instance.AutoScale()
}

func (d *Dispatcher) AutoScale() *Dispatcher {
	d.mu.Lock()
	if len(d.workers) > d.workerCount {
		d.mu.Unlock()
		d.DownScale(d.workerCount)
	} else if len(d.workers) < d.workerCount {
		d.mu.Unlock()
		d.UpScale(d.workerCount)
	} else {
		d.mu.Unlock()
	}
	return d
}

func StartWorkerObserver() *Dispatcher {
	return instance.StartWorkerObserver()
}

func (d *Dispatcher) StartWorkerObserver() *Dispatcher {
	go func() {
		for {
			select {
			case <-d.ctx.Done():
				return
			default:
				if d.workerCount != len(d.workers) && !d.scaling {
					d.AutoScale()
				}
			}
		}
	}()
	return d
}

func Reset() *Dispatcher {
	instance = instance.Reset()
	return instance
}

func (d *Dispatcher) Reset() *Dispatcher {
	d.Stop(true)
	d = New(d.workerCount)
	return d
}

func SafeReset() *Dispatcher {
	instance = instance.SafeReset()
	return instance
}

func (d *Dispatcher) SafeReset() *Dispatcher {
	for {
		if !d.scaling {
			d.Stop(true)
			d = New(d.workerCount)
			return d
		}
	}
}

func StartWithContext(c context.Context) *Dispatcher {
	return instance.StartWithContext(c)
}

func (d *Dispatcher) StartWithContext(c context.Context) *Dispatcher {
	ctx, cancel := context.WithCancel(c)
	d.ctx = ctx
	d.cancel = cancel
	for i, w := range d.workers {
		if !w.running {
			d.workers[i].start(d.ctx)
		}
	}
	d.running = true
	return d
}

func Start() *Dispatcher {
	return instance.Start()
}

func (d *Dispatcher) Start() *Dispatcher {
	return d.StartWithContext(context.Background())
}

func Add(job func() error) chan error {
	return instance.Add(job)
}

func (d *Dispatcher) Add(job func() error) chan error {
	ech := make(chan error, 1)
	d.wg.Add(1)
	d.qin <- func() {
		ech <- job()
	}
	return ech
}

func Wait() {
	instance.Wait()
}

func (d *Dispatcher) Wait() {
	if d.running {
		d.wg.Wait()
	}
}

func Stop(immediately bool) *Dispatcher {
	return instance.Stop(immediately)
}

func (d *Dispatcher) Stop(immediately bool) *Dispatcher {
	if !d.running {
		return d
	}

	if !immediately {
		glg.Warn("waiting")
		d.Wait()
	}

	d.cancel()

	d.running = false
	d = New(len(d.workers))
	return d
}

func (w *worker) start(ctx context.Context) {
	w.running = true
	go func() {
		for {
			select {
			case <-w.kill:
				return
			case <-ctx.Done():
				return
			case job := <-w.dis.qout:
				w.run(job)
			}
		}
	}()
}

func (w *worker) run(job func()) {
	defer w.dis.wg.Done()
	if job != nil {
		job()
	}
}

func (w *worker) stop() {
	w.kill <- struct{}{}
	w.running = false
}
