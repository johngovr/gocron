package gocron

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"
)

const (
	// RescheduleMode - the default is that if a limit on maximum
	// concurrent jobs is set and the limit is reached, a job will
	// skip it's run and try again on the next occurrence in the schedule
	RescheduleMode limitMode = iota

	// WaitMode - if a limit on maximum concurrent jobs is set
	// and the limit is reached, a job will wait to try and run
	// until a spot in the limit is freed up.
	//
	// Note: this mode can produce unpredictable results as
	// job execution order isn't guaranteed. For example, a job that
	// executes frequently may pile up in the wait queue and be executed
	// many times back to back when the queue opens.
	//
	// Warning: do not use this mode if your jobs will continue to stack
	// up beyond the ability of the limit workers to keep up. An example of
	// what NOT to do:
	//
	//     s.Every("1s").Do(func() {
	//         // this will result in an ever-growing number of goroutines
	//    	   // blocked trying to send to the buffered channel
	//         time.Sleep(10 * time.Minute)
	//     })
	WaitMode
)

var limitModeRfCounter = *atomic.NewInt64(0)

type executor struct {
	jobFunctions  chan jobFunction   // the chan upon which the jobFunctions are passed in from the scheduler
	ctx           context.Context    // used to tell the executor to stop
	cancel        context.CancelFunc // used to tell the executor to stop
	wg            *sync.WaitGroup    // used by the scheduler to wait for the executor to stop
	jobsWg        *sync.WaitGroup    // used by the executor to wait for all jobs to finish
	singletonWgs  *sync.Map          // used by the executor to wait for the singleton runners to complete
	skipExecution *atomic.Bool       // used to pause the execution of jobs

	limitMode               limitMode        // when SetMaxConcurrentJobs() is set upon the scheduler
	limitModeMaxRunningJobs int              // stores the maximum number of concurrently running jobs
	limitModeFuncsRunning   *atomic.Int64    // tracks the count of limited mode funcs running
	limitModeFuncWg         *sync.WaitGroup  // allow the executor to wait for limit mode functions to wrap up
	limitModeQueue          chan jobFunction // pass job functions to the limit mode workers
	limitModeQueueMu        *sync.Mutex      // for protecting the limitModeQueue
	limitModeRunningJobs    *atomic.Int64    // tracks the count of running jobs to check against the max
	stopped                 *atomic.Bool     // allow workers to drain the buffered limitModeQueue

	distributedLocker  Locker  // support running jobs across multiple instances
	distributedElector Elector // support running jobs across multiple instances

	// limitModeRfCount    *atomic.Int64 // runnint function counter
	limitModeQueueStore *jbfStore // store jobFunction in list
}

type jbfStore struct {
	jbfs []*jobFunction // job function slice
	len  int            // number
}

func newJbfStore() *jbfStore {
	rst := &jbfStore{
		len:  0,
		jbfs: make([]*jobFunction, 0),
	}
	return rst
}
func (s *jbfStore) Len() int { return s.len }

// only save nameed job
func (s *jbfStore) Store(jbf *jobFunction) {
	fmt.Println(" store job function name : ", jbf.jobName)
	if fj := s.Find(jbf.jobName); fj == nil {
		s.jbfs = append(s.jbfs, jbf)
		s.len++
	}
}

func (s *jbfStore) Find(name string) *jobFunction {
	var rst *jobFunction = nil
	for _, j := range s.jbfs {
		if strings.EqualFold(name, j.jobName) {
			fmt.Println(" find job function name ok :", name)
			rst = j
			break
		}
	}
	return rst
}

// pick one highest priority job function
func (s *jbfStore) Pick() *jobFunction {
	var rst *jobFunction = nil
	var fj, p int = 0, -1

	for i, jbf := range s.jbfs {
		if jbf.priority > p {
			fj = i
			rst = jbf
			p = jbf.priority
		}
	}
	if rst != nil {
		s.jbfs = append(s.jbfs[0:fj], s.jbfs[fj+1:]...)
		s.len = len(s.jbfs)
	}
	if rst != nil {
		fmt.Println(" pick job function name :", rst.jobName)
	} else {
		fmt.Println(" not find job function in the store.")
	}

	return rst
}

func newExecutor() executor {
	e := executor{
		jobFunctions:          make(chan jobFunction, 1),
		singletonWgs:          &sync.Map{},
		limitModeFuncsRunning: atomic.NewInt64(0),
		limitModeFuncWg:       &sync.WaitGroup{},
		limitModeRunningJobs:  atomic.NewInt64(0),
		limitModeQueueMu:      &sync.Mutex{},

		limitModeQueueStore: newJbfStore(),
	}
	return e
}

func runJob(f jobFunction) {
	defer traceMsg(" runJob :" + f.jobName)()
	limitModeRfCounter.Inc()
	defer limitModeRfCounter.Dec()

	panicHandlerMutex.RLock()
	defer panicHandlerMutex.RUnlock()

	if panicHandler != nil {
		defer func() {
			if r := recover(); r != nil {
				panicHandler(f.funcName, r)
			}
		}()
	}
	f.runStartCount.Add(1)
	f.isRunning.Store(true)
	callJobFunc(f.eventListeners.onBeforeJobExecution)
	_ = callJobFuncWithParams(f.eventListeners.beforeJobRuns, []interface{}{f.getName()})
	err := callJobFuncWithParams(f.function, f.parameters)
	if err != nil {
		_ = callJobFuncWithParams(f.eventListeners.onError, []interface{}{f.getName(), err})
	} else {
		_ = callJobFuncWithParams(f.eventListeners.noError, []interface{}{f.getName()})
	}
	_ = callJobFuncWithParams(f.eventListeners.afterJobRuns, []interface{}{f.getName()})
	callJobFunc(f.eventListeners.onAfterJobExecution)
	f.isRunning.Store(false)
	f.runFinishCount.Add(1)
}

func (jf *jobFunction) singletonRunner() {
	defer traceMsg(" job function singletonRunner, jobName:" + jf.jobName)() // .jobName)() //jf.getName())()

	jf.singletonRunnerOn.Store(true)
	jf.singletonWgMu.Lock()
	jf.singletonWg.Add(1)
	jf.singletonWgMu.Unlock()
	for {
		select {
		case <-jf.ctx.Done():
			logMsg(" singletonRunner -> : <-jf.ctx.Done() :" + jf.jobName)

			jf.singletonWg.Done()
			jf.singletonRunnerOn.Store(false)
			jf.singletonQueueMu.Lock()
			jf.singletonQueue = make(chan struct{}, 1000)
			jf.singletonQueueMu.Unlock()
			jf.stopped.Store(false)
			return
		case <-jf.singletonQueue:
			logMsg(" singletonRunner -> : <-jf.singletonQueue :" + jf.jobName)
			if !jf.stopped.Load() {
				logMsg(" singletonRunner -> : before call runJob(*jf), jobName:" + jf.jobName)
				runJob(*jf)
			}
		}
	}
}

func (e *executor) limitModeRunner() {
	defer traceMsg(" limitModeRunner")()

	for {
		select {
		case <-e.ctx.Done():
			logMsg(" limitModeRunner -> : <-e.ctx.Done()")

			e.limitModeFuncsRunning.Dec() // .Inc() // e.limitModeFuncsRunning.Inc()
			e.limitModeFuncWg.Done()
			return
		case jf := <-e.limitModeQueue:
			logMsg("  limitModeRunner -> : jf := <-e.limitModeQueue :" + jf.jobName)

			if !e.stopped.Load() {
				select {
				case <-jf.ctx.Done():
					logMsg(" limitModeRunner -> : jf := <-e.limitModeQueue :-> <-jf.ctx.Done() :" + jf.jobName)
				default:
					logMsg(" limitModeRunner -> : jf := <-e.limitModeQueue, default: before e.runJob(jf) :" + jf.jobName)
					e.runJob(jf)
				}
			}
		}
	}
}

func (e *executor) start() {
	defer traceMsg(" executor start.")()

	e.wg = &sync.WaitGroup{}
	e.wg.Add(1)

	stopCtx, cancel := context.WithCancel(context.Background())
	e.ctx = stopCtx
	e.cancel = cancel

	e.jobsWg = &sync.WaitGroup{}

	e.stopped = atomic.NewBool(false)
	e.skipExecution = atomic.NewBool(false)

	e.limitModeQueueMu.Lock()
	e.limitModeQueue = make(chan jobFunction, 1000)
	e.limitModeQueueMu.Unlock()

	go e.run()
}

func (e *executor) runJob(f jobFunction) {
	defer traceMsg(" executor runJob :" + f.jobName)()

	defer func() {
		if e.limitMode == RescheduleMode && e.limitModeMaxRunningJobs > 0 {
			e.limitModeRunningJobs.Add(-1)
		}
	}()
	switch f.runConfig.mode {
	case defaultMode:
		lockKey := f.jobName
		if lockKey == "" {
			lockKey = f.funcName
		}
		if e.distributedElector != nil {
			err := e.distributedElector.IsLeader(e.ctx)
			if err != nil {
				return
			}
			runJob(f)
			return
		}
		if e.distributedLocker != nil {
			l, err := e.distributedLocker.Lock(f.ctx, lockKey)
			if err != nil || l == nil {
				return
			}
			defer func() {
				durationToNextRun := time.Until(f.jobFuncNextRun)
				if durationToNextRun > time.Second*5 {
					durationToNextRun = time.Second * 5
				}

				delay := time.Duration(float64(durationToNextRun) * 0.9)
				if e.limitModeMaxRunningJobs > 0 {
					time.AfterFunc(delay, func() {
						_ = l.Unlock(f.ctx)
					})
					return
				}

				if durationToNextRun > time.Millisecond*100 {
					timer := time.NewTimer(delay)
					defer timer.Stop()

					select {
					case <-e.ctx.Done():
					case <-timer.C:
					}
				}
				_ = l.Unlock(f.ctx)
			}()
			runJob(f)
			return
		}
		runJob(f)
	case singletonMode:
		logMsg(" executor runJob : -> singleton mode, jobName:" + f.jobName) // f.getName())

		e.singletonWgs.Store(f.singletonWg, f.singletonWgMu)

		if !f.singletonRunnerOn.Load() {
			logMsg(" executor runJob : -> singleton mode before call f.singletonRunner() :" + f.jobName)
			go f.singletonRunner()
		}
		f.singletonQueueMu.Lock()
		f.singletonQueue <- struct{}{}
		f.singletonQueueMu.Unlock()
	}
}

func (e *executor) run() {
	defer traceMsg(" executor run.")()
	limitModeRfCounter.Store(0)

	trf := func() {
		if int(limitModeRfCounter.Load()) < e.limitModeMaxRunningJobs {
			if e.limitModeQueueStore.Len() > 0 {
				jf := e.limitModeQueueStore.Pick()
				if jf == nil {
					return
				}
				e.limitModeQueue <- *jf
				time.Sleep(1 * time.Millisecond)
			}
		}
	}
	tr := time.AfterFunc(500*time.Millisecond, trf)
	defer tr.Stop()

	for {
		select {
		case f := <-e.jobFunctions:
			logMsg(" executor run. -> f := <-e.jobFunctions begin :" + f.jobName)

			if e.stopped.Load() || e.skipExecution.Load() {
				continue
			}

			// logMsg(" executor run : -> f := <-e.jobFunctions -> e.limitModeMaxRunningJobs > 0 :" + f.jobName)
			if e.limitModeMaxRunningJobs > 0 {
				countRunning := e.limitModeFuncsRunning.Load()
				if countRunning < int64(e.limitModeMaxRunningJobs) {
					diff := int64(e.limitModeMaxRunningJobs) - countRunning
					for i := int64(0); i < diff; i++ {
						e.limitModeFuncWg.Add(1)

						logMsg(" executor run : ->  f := <-e.jobFunctions -> diff :" + fmt.Sprint(diff) + ", before call e.limitModeRunner() :" + f.jobName)
						go e.limitModeRunner()
						e.limitModeFuncsRunning.Inc()
					}
				}
			}

			e.jobsWg.Add(1)
			go func() {
				defer traceMsg(" executor run, go func() begin :" + f.jobName)()

				defer e.jobsWg.Done()

				if e.limitModeMaxRunningJobs > 0 {
					switch e.limitMode {
					case RescheduleMode:
						if e.limitModeRunningJobs.Load() < int64(e.limitModeMaxRunningJobs) {
							select {
							case e.limitModeQueue <- f:
								logMsg(" executor run, go func() ->RescheduleMode -> e.limitModeQueue <- f :" + f.jobName)
								e.limitModeRunningJobs.Inc()
							case <-e.ctx.Done():
								logMsg(" executor run, go func() ->RescheduleMode -> <-e.ctx.Done() :" + f.jobName)
							}
						}
					case WaitMode:
						// store the f jobFunction
						// if running function number less than MaxRunning limit then push jobfunction into queue
						e.limitModeQueueStore.Store(&f)
						trf()
						/*
							select {
							case e.limitModeQueue <- f:
								logMsg(" executor run, go func() ->WaitMode -> e.limitModeQueue <- f :" + f.jobName)
							case <-e.ctx.Done():
								logMsg(" executor run, go func() ->WaitMode -> <-e.ctx.Done() :" + f.jobName)
							}
						*/
					}
					return
				}

				logMsg(" executor run, go func() before call e.runJob(f) :" + f.jobName)
				e.runJob(f)
			}()
		case <-e.ctx.Done():
			logMsg(" executor run. -> <-e.ctx.Done()")
			e.jobsWg.Wait()
			e.wg.Done()
			return
		}
	}
}

func (e *executor) stop() {
	defer traceMsg(" stopping executor.")()

	e.stopped.Store(true)
	e.cancel()
	e.wg.Wait()
	if e.singletonWgs != nil {
		e.singletonWgs.Range(func(key, value interface{}) bool {
			wg, wgOk := key.(*sync.WaitGroup)
			mu, muOk := value.(*sync.Mutex)
			if wgOk && muOk {
				mu.Lock()
				wg.Wait()
				mu.Unlock()
			}
			return true
		})
	}
	if e.limitModeMaxRunningJobs > 0 {
		e.limitModeFuncWg.Wait()
		e.limitModeQueueMu.Lock()
		e.limitModeQueue = nil
		e.limitModeQueueMu.Unlock()

		e.limitModeQueueStore = nil
	}
}
